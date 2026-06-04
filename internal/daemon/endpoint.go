package daemon

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
)

// MaxUnixPathLen is the platform socket path length limit.
var MaxUnixPathLen = kitdaemon.MaxUnixPathLen

// DaemonEndpoint encapsulates the transport type and address for the daemon.
type DaemonEndpoint struct {
	Network string // "tcp" or "unix"
	Address string // "127.0.0.1:7373" or "/tmp/roborev-1000/daemon.sock"
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
	return e.kitEndpoint().BaseURL()
}

// HTTPClient returns an http.Client configured for this endpoint's transport.
func (e DaemonEndpoint) HTTPClient(timeout time.Duration) *http.Client {
	return e.kitEndpoint().HTTPClient(kitdaemon.HTTPClientOptions{
		Timeout:           timeout,
		DisableKeepAlives: e.IsUnix(),
	})
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
