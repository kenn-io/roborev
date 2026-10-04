package daemon

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// MaxUnixPathLen is the platform socket path length limit.
var MaxUnixPathLen = kitdaemon.MaxUnixPathLen

// DaemonEndpoint encapsulates the transport type and address for the daemon.
type DaemonEndpoint struct {
	Network   string // "tcp" or "unix"
	remoteURL string // Explicit HTTPS URL; never used for local discovery.
	accessErr error  // A terminal discovery error; never persisted.
	Address   string // "127.0.0.1:7373" or "/tmp/roborev-1000/daemon.sock"
}

func (e DaemonEndpoint) kitEndpoint() kitdaemon.Endpoint {
	return kitdaemon.Endpoint{Network: e.Network, Address: e.Address}
}

func daemonEndpointFromKit(ep kitdaemon.Endpoint) DaemonEndpoint {
	return DaemonEndpoint{Network: ep.Network, Address: ep.Address}
}

// ParseEndpoint parses a server_addr config value into a DaemonEndpoint.
func ParseEndpoint(serverAddr string) (DaemonEndpoint, error) {
	if strings.HasPrefix(serverAddr, "https://") {
		cfg, err := config.LoadRemoteClient()
		if err != nil {
			return DaemonEndpoint{}, err
		}
		// An exact explicit credential binding selects remote trust. All other
		// endpoints retain the existing local transport/runtime policy.
		if cfg.MatchOrigin(serverAddr) == nil {
			u, _ := requestsigning.ValidateBase(serverAddr)
			return DaemonEndpoint{Network: "https", Address: u.Host, remoteURL: u.String()}, nil
		}
	}
	raw := serverAddr
	if raw == "" {
		raw = "127.0.0.1:7373"
	}

	ep, err := kitdaemon.ParseEndpoint(raw, kitdaemon.ParseEndpointOptions{
		DefaultTCPAddress: "127.0.0.1:7373",
		DefaultUnixPath:   DefaultSocketPath(),
		TCPPolicy:         kitdaemon.RequireLoopback,
	})
	if err != nil {
		if !strings.HasPrefix(raw, "unix://") {
			return DaemonEndpoint{}, fmt.Errorf(
				"daemon address %q must use a loopback host (127.0.0.1, localhost, or [::1]): %w",
				raw, err)
		}
		return DaemonEndpoint{}, err
	}
	return daemonEndpointFromKit(ep), nil
}

// DefaultSocketPath returns the auto-generated socket path under os.TempDir(),
// or $XDG_RUNTIME_DIR when a safe path is available.
func DefaultSocketPath() string {
	return kitdaemon.DefaultSocketPath(daemonServiceName)
}

// IsUnix returns true if this endpoint uses a Unix domain socket.
func (e DaemonEndpoint) IsUnix() bool {
	return e.kitEndpoint().IsUnix()
}

// BaseURL returns the HTTP base URL for constructing API requests.
func (e DaemonEndpoint) BaseURL() string {
	if e.IsRemote() {
		return e.remoteURL
	}
	return e.kitEndpoint().BaseURL()
}

// HTTPClient returns an http.Client configured for this endpoint's transport.
func (e DaemonEndpoint) HTTPClient(timeout time.Duration) *http.Client {
	if e.IsRemote() {
		return e.remoteHTTPClient(timeout)
	}
	client := auth.HTTPClient(e.BaseURL(), e.transportClient(timeout), func() (string, error) {
		if e.accessErr != nil {
			return "", e.accessErr
		}
		return loadClientAuthKey()
	})
	if obs := clientResponseObserver.Load(); obs != nil {
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		client.Transport = observedTransport{base: transport, ep: e, fn: *obs}
	}
	return client
}

func (e DaemonEndpoint) transportClient(timeout time.Duration) *http.Client {
	return e.kitEndpoint().HTTPClient(kitdaemon.HTTPClientOptions{
		Timeout:           timeout,
		DisableKeepAlives: e.IsUnix(),
	})
}

// clientResponseObserver, when set by the CLI, sees each response a client from HTTPClient receives.
var clientResponseObserver atomic.Pointer[func(DaemonEndpoint, *http.Request, *http.Response)]

// SetClientResponseObserver installs fn for clients built after the call; nil removes it. The daemon and the TUI never set it.
func SetClientResponseObserver(fn func(DaemonEndpoint, *http.Request, *http.Response)) {
	if fn == nil {
		clientResponseObserver.Store(nil)
		return
	}
	clientResponseObserver.Store(&fn)
}

type observedTransport struct {
	base http.RoundTripper
	ep   DaemonEndpoint
	fn   func(DaemonEndpoint, *http.Request, *http.Response)
}

// RoundTrip hands every response back untouched; the observer must not read or close the body.
func (t observedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		t.fn(t.ep, req, resp)
	}
	return resp, err
}

// Listener creates a net.Listener bound to this endpoint.
func (e DaemonEndpoint) Listener() (net.Listener, error) {
	if e.IsRemote() {
		return nil, fmt.Errorf("remote endpoint cannot create a local listener")
	}
	return e.kitEndpoint().Listen()
}

// String returns a human-readable representation for logging.
func (e DaemonEndpoint) String() string {
	return e.Network + ":" + e.Address
}

// ConfigAddr returns a ParseEndpoint-compatible string suitable for
// persisting in config or runtime metadata files.
func (e DaemonEndpoint) ConfigAddr() string {
	return e.kitEndpoint().ConfigAddress()
}

// Port returns the TCP port, or 0 for Unix sockets.
func (e DaemonEndpoint) Port() int {
	return e.kitEndpoint().Port()
}

// APIClient returns the generated API client using this endpoint's transport.
func (e DaemonEndpoint) APIClient(timeout time.Duration) *roborevclient.Client {
	api, err := roborevclient.NewWithHTTPClient(e.BaseURL(), e.HTTPClient(timeout))
	if err != nil {
		panic(fmt.Sprintf("create daemon API client: %v", err))
	}
	return api
}
