//go:build windows

package daemon

import (
	"errors"
	"net"
)

// unixSocketsSupported reports whether the daemon can serve its private
// Unix socket next to a TCP listener.
const unixSocketsSupported = false

var errUnixSocketsUnsupported = errors.New("unix sockets are not supported on Windows")

func listenUnixEndpoint(DaemonEndpoint) (net.Listener, error) {
	return nil, errUnixSocketsUnsupported
}

func listenAuxiliaryEndpoint(DaemonEndpoint) (net.Listener, *DaemonEndpoint, error) {
	return nil, nil, nil
}
