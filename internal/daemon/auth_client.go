package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/internal/config"
)

// ErrClientConfig means the global configuration could not be loaded.
var ErrClientConfig = errors.New("cannot load daemon client config")

func loadClientAuth() (config.ClientAuth, error) {
	clientAuth, err := config.LoadGlobalClientAuth()
	if err != nil {
		return config.ClientAuth{}, fmt.Errorf("%w: %w", ErrClientConfig, err)
	}
	return clientAuth, nil
}

// pingHTTPClient keeps unjudged plain-TCP liveness probes independent of
// credentials, including errors loading them.
func (e DaemonEndpoint) pingHTTPClient(timeout time.Duration, settings config.ClientAuth, configErr error) *http.Client {
	if e.IsRemote() {
		return e.HTTPClient(timeout)
	}
	if e.unjudged && !e.IsUnix() && !settings.TLS.Enabled() {
		return e.transportClient(timeout, settings.TLS)
	}
	return e.authClient(timeout, settings.TLS, func() (string, error) {
		if e.accessErr != nil {
			return "", e.accessErr
		}
		return settings.Key, configErr
	})
}

// readinessHTTPClient probes a listener this process just bound. No other
// process can hold that endpoint, so the key is sent over any transport.
// Ordinary CLI and TUI clients use HTTPClient instead.
func (e DaemonEndpoint) readinessHTTPClient(timeout time.Duration, startup config.ClientAuth) *http.Client {
	return auth.HTTPClient(e.BaseURL(), e.transportClient(timeout, startup.TLS), func() (string, error) {
		return startup.Key, nil
	})
}

// WithAccessError prevents an endpoint-selection failure from falling back to
// an unrelated daemon. Clients constructed from this endpoint return err.
func (e DaemonEndpoint) WithAccessError(err error) DaemonEndpoint {
	e.accessErr = err
	return e
}

type probeAuthTransport struct{ base http.RoundTripper }

func (t probeAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: check auth_key in the global config", ErrDaemonAccessDenied)
	}
	return resp, err
}

func probeDaemonHTTP(ctx context.Context, ep DaemonEndpoint, timeout time.Duration, client *http.Client) (*PingInfo, error) {
	clone := *client
	clone.Transport = probeAuthTransport{base: client.Transport}
	info, err := kitdaemon.ProbeHTTP(ctx, &clone, ep.BaseURL(), kitdaemon.ProbeOptions{ExpectedService: daemonServiceName, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return pingInfoFromKit(info), nil
}
