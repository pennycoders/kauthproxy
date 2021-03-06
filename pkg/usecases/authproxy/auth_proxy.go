package authproxy

import (
	"context"
	"net/url"
	"strings"
	"sync"

	"github.com/cenkalti/backoff"
	"github.com/google/wire"
	"github.com/int128/kauthproxy/pkg/adaptors/browser"
	"github.com/int128/kauthproxy/pkg/adaptors/env"
	"github.com/int128/kauthproxy/pkg/adaptors/logger"
	"github.com/int128/kauthproxy/pkg/adaptors/portforwarder"
	"github.com/int128/kauthproxy/pkg/adaptors/resolver"
	"github.com/int128/kauthproxy/pkg/adaptors/reverseproxy"
	"github.com/int128/kauthproxy/pkg/adaptors/transport"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

var Set = wire.NewSet(
	wire.Struct(new(AuthProxy), "*"),
	wire.Bind(new(Interface), new(*AuthProxy)),
)

type Interface interface {
	Do(ctx context.Context, in Option) error
}

var portForwarderConnectionLostError = xerrors.New("connection lost")

// AuthProxy provides a use-case of authentication proxy.
type AuthProxy struct {
	ReverseProxy    reverseproxy.Interface
	PortForwarder   portforwarder.Interface
	ResolverFactory resolver.FactoryInterface
	NewTransport    transport.NewFunc
	Env             env.Interface
	Browser         browser.Interface
	Logger          logger.Interface
}

// Option represents an option of AuthProxy.
type Option struct {
	Config                *rest.Config
	Namespace             string
	TargetURL             *url.URL
	BindAddressCandidates []string
}

// Do runs the use-case.
// This runs a port forwarder and reverse proxy.
//
// This never returns nil.
// It returns an error which wraps context.Canceled if the context is canceled.
func (u *AuthProxy) Do(ctx context.Context, o Option) error {
	rsv, err := u.ResolverFactory.New(o.Config)
	if err != nil {
		return xerrors.Errorf("could not create a resolver: %w", err)
	}
	pod, containerPort, err := parseTargetURL(rsv, o.Namespace, o.TargetURL)
	if err != nil {
		return xerrors.Errorf("could not find the pod and container port: %w", err)
	}
	u.Logger.V(1).Infof("found container port %d of pod %s", containerPort, pod.Name)
	transitPort, err := u.Env.AllocateLocalPort()
	if err != nil {
		return xerrors.Errorf("could not allocate a local port: %w", err)
	}
	rpTransport, err := u.NewTransport(o.Config)
	if err != nil {
		return xerrors.Errorf("could not create a transport for reverse proxy: %w", err)
	}
	u.Logger.V(1).Infof("client -> reverse_proxy -> port_forwarder:%d -> pod -> container:%d", transitPort, containerPort)
	pfo := portforwarder.Option{
		Config:              o.Config,
		SourcePort:          transitPort,
		TargetPodURL:        pod.GetSelfLink(),
		TargetContainerPort: containerPort,
	}
	rpo := reverseproxy.Option{
		Transport:             rpTransport,
		BindAddressCandidates: o.BindAddressCandidates,
		TargetScheme:          o.TargetURL.Scheme,
		TargetHost:            "localhost",
		TargetPort:            transitPort,
	}
	var once sync.Once
	b := backoff.NewExponentialBackOff()
	if err := backoff.Retry(func() error {
		if err := u.run(ctx, pfo, rpo, &once); err != nil {
			if xerrors.Is(err, portForwarderConnectionLostError) {
				u.Logger.Printf("retrying: %s", err)
				return err
			}
			return backoff.Permanent(err)
		}
		return nil
	}, b); err != nil {
		return xerrors.Errorf("retry over: %w", err)
	}
	return nil
}

// run runs a port forwarder and reverse proxy, and waits for them, as follows:
//
//	1. Run a port forwarder.
//	2. When the port forwarder is ready, run a reverse proxy.
//	3. When the reverse proxy is ready, open the browser (only first time).
//
// When the context is canceled,
//
//	- Shut down the port forwarder.
//	- Shut down the reverse proxy.
//
// This never returns nil.
// It returns an error which wraps context.Canceled if the context is canceled.
// It returns portForwarderConnectionLostError if a connection has lost.
func (u *AuthProxy) run(ctx context.Context, pfo portforwarder.Option, rpo reverseproxy.Option, once *sync.Once) error {
	portForwarderIsReady := make(chan struct{})
	reverseProxyIsReady := make(chan reverseproxy.Instance, 1)
	stopPortForwarder := make(chan struct{})
	defer close(reverseProxyIsReady)

	eg, ctx := errgroup.WithContext(ctx)
	// start a port forwarder
	eg.Go(func() error {
		u.Logger.V(1).Infof("starting a port forwarder")
		if err := u.PortForwarder.Run(pfo, portForwarderIsReady, stopPortForwarder); err != nil {
			return xerrors.Errorf("could not run a port forwarder: %w", err)
		}
		u.Logger.V(1).Infof("stopped the port forwarder")
		if ctx.Err() == nil {
			u.Logger.V(1).Infof("connection of the port forwarder has lost")
			return portForwarderConnectionLostError
		}
		return nil
	})
	// stop the port forwarder when the context is done
	eg.Go(func() error {
		<-ctx.Done()
		u.Logger.V(1).Infof("stopping the port forwarder")
		close(stopPortForwarder)
		return xerrors.Errorf("context canceled while running the port forwarder: %w", ctx.Err())
	})
	// start a reverse proxy when the port forwarder is ready
	eg.Go(func() error {
		select {
		case <-portForwarderIsReady:
			u.Logger.V(1).Infof("starting a reverse proxy")
			if err := u.ReverseProxy.Run(rpo, reverseProxyIsReady); err != nil {
				return xerrors.Errorf("could not run a reverse proxy: %w", err)
			}
			u.Logger.V(1).Infof("stopped the reverse proxy")
			return nil
		case <-ctx.Done():
			u.Logger.V(1).Infof("context canceled before starting reverse proxy")
			return xerrors.Errorf("context canceled before starting reverse proxy: %w", ctx.Err())
		}
	})
	// open the browser when the reverse proxy is ready
	eg.Go(func() error {
		u.Logger.V(1).Infof("waiting for the reverse proxy")
		select {
		case rp := <-reverseProxyIsReady:
			u.Logger.V(1).Infof("the reverse proxy is ready")
			rpURL := rp.URL().String()
			u.Logger.Printf("Open %s", rpURL)
			once.Do(func() {
				if err := u.Browser.Open(rpURL); err != nil {
					u.Logger.Printf("error while opening the browser: %s", err)
				}
			})
			// shutdown the reverse proxy when the context is done
			eg.Go(func() error {
				<-ctx.Done()
				u.Logger.V(1).Infof("shutting down the reverse proxy")
				if err := rp.Shutdown(context.Background()); err != nil {
					return xerrors.Errorf("could not shutdown the reverse proxy: %w", err)
				}
				return xerrors.Errorf("context canceled while running the reverse proxy: %w", ctx.Err())
			})
			return nil
		case <-ctx.Done():
			u.Logger.V(1).Infof("context canceled before reverse proxy is ready")
			return xerrors.Errorf("context canceled before reverse proxy is ready: %w", ctx.Err())
		}
	})
	if err := eg.Wait(); err != nil {
		return xerrors.Errorf("error while running an authentication proxy: %w", err)
	}
	return nil
}

func parseTargetURL(r resolver.Interface, namespace string, u *url.URL) (*v1.Pod, int, error) {
	h := u.Hostname()
	if strings.HasSuffix(h, ".svc") {
		serviceName := strings.TrimSuffix(h, ".svc")
		return r.FindPodByServiceName(namespace, serviceName)
	}
	return r.FindPodByName(namespace, h)
}
