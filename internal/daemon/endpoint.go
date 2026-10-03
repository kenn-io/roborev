package daemon

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/auth"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// MaxUnixPathLen is the platform socket path length limit.
var MaxUnixPathLen = kitdaemon.MaxUnixPathLen

// DaemonEndpoint encapsulates the transport type and address for the daemon.
type DaemonEndpoint struct {
	Network           string // "tcp" or "unix"
	accessErr         error  // A terminal discovery error; never persisted.
	Address           string // TCP host:port or Unix socket path.
	TLSCertPEM        string // Public certificate pinned from the protected runtime record.
	allowTLSBootstrap bool   // Allows daemon startup probes before its runtime record is published.
}

func (e DaemonEndpoint) kitEndpoint() kitdaemon.Endpoint {
	return kitdaemon.Endpoint{Network: e.Network, Address: e.Address}
}

func daemonEndpointFromKit(ep kitdaemon.Endpoint) DaemonEndpoint {
	return DaemonEndpoint{Network: ep.Network, Address: ep.Address}
}

// ParseEndpoint parses a server_addr config value into a DaemonEndpoint.
func ParseEndpoint(serverAddr string) (DaemonEndpoint, error) {
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

// SameRuntimeEndpointAddress reports whether two addresses identify the same
// loopback listener, including the localhost and loopback-IP aliases.
func SameRuntimeEndpointAddress(runtimeAddress, selectedAddress string) bool {
	if runtimeAddress == selectedAddress {
		return true
	}
	runtimeHost, runtimePort, runtimeErr := net.SplitHostPort(runtimeAddress)
	selectedHost, selectedPort, selectedErr := net.SplitHostPort(selectedAddress)
	if runtimeErr != nil || selectedErr != nil || runtimePort != selectedPort {
		return false
	}
	return runtimeHost == "localhost" && (selectedHost == "127.0.0.1" || selectedHost == "::1") ||
		selectedHost == "localhost" && (runtimeHost == "127.0.0.1" || runtimeHost == "::1")
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
	baseURL := e.kitEndpoint().BaseURL()
	if e.TLSCertPEM != "" && !e.IsUnix() {
		return "https://" + strings.TrimPrefix(baseURL, "http://")
	}
	return baseURL
}

// HTTPClient returns an http.Client configured for this endpoint's transport.
func (e DaemonEndpoint) HTTPClient(timeout time.Duration) *http.Client {
	client := e.authenticatedClient(timeout, func() (string, error) {
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
	client := e.kitEndpoint().HTTPClient(kitdaemon.HTTPClientOptions{
		Timeout:           timeout,
		DisableKeepAlives: e.IsUnix(),
	})
	if e.TLSCertPEM == "" || e.IsUnix() {
		return client
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		client.Transport = errorRoundTripper{err: errors.New("unsupported daemon TLS transport")}
		return client
	}
	transport = transport.Clone()
	tlsConfig := transport.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	if _, err := parseDaemonTLSCertificate(e.TLSCertPEM); err != nil {
		client.Transport = errorRoundTripper{
			err: fmt.Errorf("%w: invalid daemon TLS certificate in runtime record", auth.ErrUnverifiedServer),
		}
		return client
	}
	// The daemon certificate rotates when the daemon process restarts. Disable
	// the static root check so VerifyConnection can replace it with an exact pin
	// from the current protected runtime record. That refresh keeps existing
	// clients working while still verifying the daemon before sending auth_key.
	tlsConfig.InsecureSkipVerify = true // #nosec G402 -- VerifyConnection performs exact runtime pinning and hostname validation.
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		return verifyCurrentDaemonCertificate(e, state)
	}
	transport.TLSClientConfig = tlsConfig
	client.Transport = transport
	return client
}

func parseDaemonTLSCertificate(certificatePEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func verifyCurrentDaemonCertificate(endpoint DaemonEndpoint, state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return auth.ErrUnverifiedServer
	}
	peer := state.PeerCertificates[0]
	host, _, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return auth.ErrUnverifiedServer
	}

	pinnedCertificates, hasCurrentRuntime, err := currentDaemonTLSCertificates(endpoint)
	if err != nil {
		return auth.ErrUnverifiedServer
	}
	if !hasCurrentRuntime {
		if !endpoint.allowTLSBootstrap {
			return auth.ErrUnverifiedServer
		}
		// Startup probes run before the daemon publishes its first runtime
		// record. Only the daemon's own readiness probe may use the certificate
		// supplied by the endpoint in that window.
		pinned, err := parseDaemonTLSCertificate(endpoint.TLSCertPEM)
		if err != nil {
			return auth.ErrUnverifiedServer
		}
		pinnedCertificates = append(pinnedCertificates, pinned)
	}
	for _, pinned := range pinnedCertificates {
		if !bytes.Equal(peer.Raw, pinned.Raw) {
			continue
		}
		roots := x509.NewCertPool()
		roots.AddCert(pinned)
		if _, err := peer.Verify(x509.VerifyOptions{
			Roots:     roots,
			DNSName:   host,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err == nil {
			return nil
		}
	}
	return auth.ErrUnverifiedServer
}

func currentDaemonTLSCertificates(endpoint DaemonEndpoint) ([]*x509.Certificate, bool, error) {
	runtimes, err := ListAllRuntimes()
	if err != nil {
		return nil, false, err
	}
	var certificates []*x509.Certificate
	hasCurrentRuntime := false
	for _, runtime := range runtimes {
		if runtime.Service != daemonServiceName || runtime.PID <= 0 || runtime.HasStaleProcess() {
			continue
		}
		for _, candidate := range runtime.Endpoints() {
			if candidate.Network != endpoint.Network || !SameRuntimeEndpointAddress(candidate.Address, endpoint.Address) {
				continue
			}
			hasCurrentRuntime = true
			if candidate.TLSCertPEM == "" {
				continue
			}
			certificate, err := parseDaemonTLSCertificate(candidate.TLSCertPEM)
			if err == nil {
				certificates = append(certificates, certificate)
			}
		}
	}
	return certificates, hasCurrentRuntime, nil
}

func (e DaemonEndpoint) authenticatedClient(timeout time.Duration, key func() (string, error)) *http.Client {
	if e.IsUnix() {
		return auth.HTTPClientWithLocalTransport(e.BaseURL(), e.transportClient(timeout), key)
	}
	return auth.HTTPClient(e.BaseURL(), e.transportClient(timeout), key)
}

type errorRoundTripper struct{ err error }

func (t errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
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
