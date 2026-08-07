//go:build !windows

package daemon

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
)

func listenUnixEndpoint(ep DaemonEndpoint) (net.Listener, error) {
	socketDir := filepath.Dir(ep.Address)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	fi, err := os.Stat(socketDir)
	if err != nil {
		return nil, fmt.Errorf("stat socket directory: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf(
			"socket directory %s has unsafe permissions %o (must not be group/world accessible)",
			socketDir, perm,
		)
	}

	_ = os.Remove(ep.Address)
	listener, err := ep.Listener()
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", ep, err)
	}
	if err := os.Chmod(ep.Address, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(ep.Address)
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return listener, nil
}

func listenAuxiliaryEndpoint(primary DaemonEndpoint) (net.Listener, *DaemonEndpoint, error) {
	if primary.IsUnix() {
		return nil, nil, nil
	}
	endpoint := DaemonEndpoint{Network: "unix", Address: auxiliarySocketPath()}
	listener, err := listenUnixEndpoint(endpoint)
	if err != nil {
		return nil, nil, err
	}
	return listener, &endpoint, nil
}

func auxiliarySocketPath() string {
	dataDir := filepath.Clean(config.DataDir())
	if absolute, err := filepath.Abs(dataDir); err == nil {
		dataDir = absolute
	}
	sum := sha256.Sum256([]byte(dataDir))
	service := fmt.Sprintf("%s-%x", daemonServiceName, sum[:8])
	return kitdaemon.DefaultSocketPath(service)
}
