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

func loadClientAuthKey() (string, error) {
	key, err := config.LoadGlobalAuthKey()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrClientConfig, err)
	}
	return key, nil
}

// HTTPClientWithAuthKey uses an explicit startup key for daemon readiness.
// Ordinary CLI and TUI clients use HTTPClient to load the global key instead.
func (e DaemonEndpoint) HTTPClientWithAuthKey(timeout time.Duration, key string) *http.Client {
	return auth.HTTPClient(e.BaseURL(), e.transportClient(timeout), func() (string, error) { return key, nil })
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
