//go:build windows

package daemon

import (
	"fmt"
	"net"
)

func listenUnixEndpoint(DaemonEndpoint) (net.Listener, error) {
	return nil, fmt.Errorf("Unix sockets are not supported on Windows")
}

func listenAuxiliaryEndpoint(DaemonEndpoint) (net.Listener, *DaemonEndpoint, error) {
	return nil, nil, nil
}
