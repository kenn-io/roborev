package daemon

import (
	"crypto/tls"
	"errors"
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
	guessed   bool   // A default address no running daemon published.
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
		// Only an exact explicit credential binding selects remote trust. Local
		// daemons never use HTTPS, so report the remote mismatch directly.
		if err := cfg.MatchOrigin(serverAddr); err != nil {
			return DaemonEndpoint{}, fmt.Errorf("remote daemon address %q: %w", serverAddr, err)
		}
		u, _ := requestsigning.ValidateBase(serverAddr)
		return DaemonEndpoint{Network: "https", Address: u.Host, remoteURL: u.String()}, nil
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

// ErrGuessedEndpointAuth means a client refused to send auth_key over plain
// TCP to a default address that no running daemon published. While the
// daemon is stopped, another local account could listen there.
var ErrGuessedEndpointAuth = errors.New(
	"no running daemon published this address; refusing to send auth_key over plain TCP to a default address",
)

// AsGuess marks a default address used when no runtime record names a
// running daemon. Clients do not send auth_key to it over plain TCP.
func (e DaemonEndpoint) AsGuess() DaemonEndpoint {
	e.guessed = true
	return e
}

// HTTPClient returns an http.Client configured for this endpoint's transport.
// It reads auth_key for every request and [daemon_tls] when it is built.
func (e DaemonEndpoint) HTTPClient(timeout time.Duration) *http.Client {
	if e.IsRemote() {
		return e.remoteHTTPClient(timeout)
	}
	// A load error here reappears from the per-request key load below.
	settings, _ := loadClientAuth()
	client := e.authClient(timeout, settings.TLS, func() (string, error) {
		if e.accessErr != nil {
			return "", e.accessErr
		}
		current, err := loadClientAuth()
		return current.Key, err
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

// authClient sends auth_key over plain TCP only to an address a running
// daemon published or the user chose. A guessed default address gets the key
// only over the private Unix socket or mutual TLS.
func (e DaemonEndpoint) authClient(
	timeout time.Duration,
	tlsSettings config.DaemonTLSConfig,
	key func() (string, error),
) *http.Client {
	protected := !e.guessed || e.IsUnix() || tlsSettings.Enabled()
	return auth.HTTPClient(e.BaseURL(), e.transportClient(timeout, tlsSettings), func() (string, error) {
		value, err := key()
		if err == nil && value != "" && !protected {
			return "", ErrGuessedEndpointAuth
		}
		return value, err
	})
}

func (e DaemonEndpoint) transportClient(timeout time.Duration, tlsSettings config.DaemonTLSConfig) *http.Client {
	client := e.kitEndpoint().HTTPClient(kitdaemon.HTTPClientOptions{
		Timeout:           timeout,
		DisableKeepAlives: e.IsUnix(),
	})
	if e.IsUnix() || !tlsSettings.Enabled() {
		return client
	}
	tlsConfig, err := clientTLSConfig(tlsSettings)
	transport, ok := client.Transport.(*http.Transport)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("unsupported daemon transport")
		}
		client.Transport = errorTransport{err: fmt.Errorf("%w: %w", ErrClientConfig, err)}
		return client
	}
	// The shared daemon kit builds http:// endpoint URLs, so TLS wraps each TCP
	// connection when it is dialed. The dialer verifies the address's host.
	transport.DialContext = (&tls.Dialer{Config: tlsConfig}).DialContext
	return client
}

type errorTransport struct{ err error }

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
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
