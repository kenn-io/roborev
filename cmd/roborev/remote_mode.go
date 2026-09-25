package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

// remoteEndpoint is set when the CLI talks to a daemon on another machine,
// through [remote] server or a non-loopback --server http://host:port.
// It is resolved lazily, on the first path that contacts a daemon, so a bad
// [remote] server does not break commands that never contact one (such as
// roborev config, which the user needs to repair it).
var (
	remoteEndpoint *daemon.DaemonEndpoint
	remoteResolved bool
	remoteErr      error
)

var probeRemoteDaemon = daemon.ProbeRemoteDaemonPing

// resetRemoteMode forgets the resolved remote endpoint so the next daemon
// path resolves it again.
func resetRemoteMode() {
	remoteEndpoint, remoteResolved, remoteErr = nil, false, nil
}

// setRemoteEndpoint fixes the endpoint choice without reading config: a
// remote endpoint, or nil for a local one.
func setRemoteEndpoint(ep *daemon.DaemonEndpoint) {
	remoteEndpoint, remoteResolved, remoteErr = ep, true, nil
}

// resolveRemoteMode resolves remote mode once and returns the error, if any.
func resolveRemoteMode() error {
	if !remoteResolved {
		remoteEndpoint, remoteErr = loadRemoteEndpoint()
		remoteResolved = true
	}
	return remoteErr
}

// isRemoteMode reports whether the CLI talks to a remote daemon. A remote
// config that fails to resolve returns its error: it is neither remote nor
// local mode, and callers must not fall back to the local daemon.
func isRemoteMode() (bool, error) {
	if err := resolveRemoteMode(); err != nil {
		return false, err
	}
	return remoteEndpoint != nil, nil
}

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

// loadRemoteEndpoint picks remote mode from --server, or from
// [remote] server when --server is unset.
func loadRemoteEndpoint() (*daemon.DaemonEndpoint, error) {
	if serverAddr != "" {
		if !strings.HasPrefix(serverAddr, "http://") {
			return nil, nil
		}
		ep, remote, err := parseRemoteServer(serverAddr)
		if err != nil {
			return nil, fmt.Errorf("invalid --server: %w", err)
		}
		if !remote {
			return nil, nil
		}
		return &ep, nil
	}
	configured, err := config.LoadRemoteServer()
	if err != nil {
		return nil, fmt.Errorf("%w; fix it with roborev config set --global remote.server <url>", err)
	}
	if configured == "" {
		return nil, nil
	}
	ep, remote, err := parseRemoteServer(configured)
	if err != nil {
		return nil, fmt.Errorf("invalid [remote] server in %s: %w; fix it with roborev config set --global remote.server <url>",
			config.GlobalConfigPath(), err)
	}
	if !remote {
		return nil, nil
	}
	return &ep, nil
}

func requireLocalDaemon(command string) error {
	if err := resolveRemoteMode(); err != nil {
		return err
	}
	if remoteEndpoint == nil {
		return nil
	}
	return fmt.Errorf("%s needs a local daemon; the remote daemon at http://%s cannot run it",
		command, remoteEndpoint.Address)
}

// ensureAgentHookDaemon makes sure the daemon on this machine is running for
// an agent hook. Agent hooks always talk to the local daemon. In remote mode,
// and when [remote] server is broken, they only look for a running one: a
// remote client never starts, restarts, or stops a local daemon.
func ensureAgentHookDaemon() error {
	if remote, err := isRemoteMode(); err == nil && !remote {
		return ensureThisMachineDaemon()
	}
	if _, err := getAnyRunningDaemon(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrDaemonNotRunning
		}
		return err
	}
	return nil
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
	_, err := probeRemoteDaemon(*remoteEndpoint, 2*time.Second)
	if statusErr, ok := errors.AsType[*daemon.PingStatusError](err); ok {
		if statusErr.Reason != "" {
			return fmt.Errorf("remote daemon at http://%s refused the request: %s",
				remoteEndpoint.Address, statusErr.Reason)
		}
		return fmt.Errorf("remote daemon at http://%s refused the request with HTTP %d",
			remoteEndpoint.Address, statusErr.StatusCode)
	}
	if err != nil {
		return fmt.Errorf("remote daemon at http://%s is not reachable: %w", remoteEndpoint.Address, err)
	}
	return nil
}

// repoFilterValue is the repo filter to send for a local checkout: its path
// for a local daemon, its identity for a remote one.
func repoFilterValue(root string) (string, error) {
	remote, err := isRemoteMode()
	if err != nil {
		return "", err
	}
	if !remote {
		return root, nil
	}
	return remoteRepoIdentity(root)
}

// remoteRepoRoot returns the daemon-side root path of the registered repo
// whose identity matches the local checkout at root.
func remoteRepoRoot(ctx context.Context, ep daemon.DaemonEndpoint, root string) (string, error) {
	identity, err := remoteRepoIdentity(root)
	if err != nil {
		return "", err
	}
	resp, err := ep.APIClient(10*time.Second).ListReposRaw(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list repos on remote daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("list repos on remote daemon: %s (read body: %w)", resp.Status, err)
		}
		return "", fmt.Errorf("list repos on remote daemon: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var body struct {
		Repos []storage.RepoWithCount `json:"repos"`
	}
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		return "", fmt.Errorf("decode remote repo list: %w", err)
	}
	var matches []string
	for _, repo := range body.Repos {
		if repo.Identity == identity {
			matches = append(matches, repo.RootPath)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("repo %s is not registered on the remote daemon; run roborev init on the daemon host", identity)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("repo %s matches several remote daemon checkouts: %s", identity, strings.Join(matches, ", "))
	}
}
