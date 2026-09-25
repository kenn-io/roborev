package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
)

// remoteEndpoint is set when the CLI talks to a daemon on another machine,
// through [remote] server or a non-loopback --server http://host:port.
var remoteEndpoint *daemon.DaemonEndpoint

var probeRemoteDaemon = daemon.ProbeRemoteDaemonPing

func isRemoteMode() bool { return remoteEndpoint != nil }

// parseRemoteServer parses an http://host:port remote daemon URL. Loopback
// hosts return ok=false so they keep today's local behavior.
func parseRemoteServer(raw string) (daemon.DaemonEndpoint, bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return daemon.DaemonEndpoint{}, false,
			fmt.Errorf("remote server %q must look like http://host:port", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return daemon.DaemonEndpoint{}, false, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return daemon.DaemonEndpoint{}, false, nil
	}
	if u.Port() == "" {
		return daemon.DaemonEndpoint{}, false, fmt.Errorf("remote server %q needs a port", raw)
	}
	return daemon.DaemonEndpoint{Network: "tcp", Address: u.Host}, true, nil
}

// resolveRemoteEndpoint picks remote mode from --server, or from
// [remote] server when --server is unset.
func resolveRemoteEndpoint() error {
	remoteEndpoint = nil
	raw := serverAddr
	if raw == "" {
		configured, err := config.LoadRemoteServer()
		if err != nil {
			return err
		}
		if configured == "" {
			return nil
		}
		raw = configured
	} else if !strings.HasPrefix(raw, "http://") {
		return nil
	}
	ep, remote, err := parseRemoteServer(raw)
	if err != nil {
		return err
	}
	if remote {
		remoteEndpoint = &ep
	}
	return nil
}

func requireLocalDaemon(command string) error {
	if remoteEndpoint == nil {
		return nil
	}
	return fmt.Errorf("%s needs a local daemon; the remote daemon at http://%s cannot run it",
		command, remoteEndpoint.Address)
}

// ensureLocalDaemon is ensureDaemon for commands that only work against a
// daemon on this machine.
func ensureLocalDaemon(command string) error {
	if err := requireLocalDaemon(command); err != nil {
		return err
	}
	return ensureDaemon()
}

func ensureRemoteDaemon() error {
	if _, err := probeRemoteDaemon(*remoteEndpoint, 2*time.Second); err != nil {
		return fmt.Errorf("remote daemon at http://%s is not reachable: %w", remoteEndpoint.Address, err)
	}
	return nil
}
