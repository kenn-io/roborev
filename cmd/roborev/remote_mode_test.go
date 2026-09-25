package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestParseRemoteServer(t *testing.T) {
	tests := []struct {
		raw        string
		wantRemote bool
		wantAddr   string
		wantErr    string
	}{
		{raw: "http://daemon-host.example:7474", wantRemote: true, wantAddr: "daemon-host.example:7474"},
		{raw: "http://100.64.0.5:7474", wantRemote: true, wantAddr: "100.64.0.5:7474"},
		{raw: "http://127.0.0.1:7373"},
		{raw: "http://localhost:7373"},
		{raw: "http://[::1]:7373"},
		{raw: "https://daemon-host.example:7474", wantErr: "must look like http://host:port"},
		{raw: "http://daemon-host.example", wantErr: "needs a port"},
		{raw: "http://daemon-host.example:7474/api", wantErr: "must look like http://host:port"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			ep, remote, err := parseRemoteServer(tt.raw)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantRemote, remote)
			if tt.wantRemote {
				assert.Equal(t, daemon.DaemonEndpoint{Network: "tcp", Address: tt.wantAddr}, ep)
			}
		})
	}
}

// withRemoteState saves and restores the global state remote mode touches,
// and points ROBOREV_DATA_DIR at an empty directory.
func withRemoteState(t *testing.T) {
	t.Helper()
	origServer, origParsed := serverAddr, parsedServerEndpoint
	origRemote, origResolved, origErr := remoteEndpoint, remoteResolved, remoteErr
	origStart, origRestart, origProbe := startDaemonForEnsure, restartDaemonForEnsure, probeRemoteDaemon
	origEnsureProbe, origDiscover := probeDaemonForEnsure, getAnyRunningDaemon
	t.Cleanup(func() {
		serverAddr, parsedServerEndpoint = origServer, origParsed
		remoteEndpoint, remoteResolved, remoteErr = origRemote, origResolved, origErr
		startDaemonForEnsure, restartDaemonForEnsure, probeRemoteDaemon = origStart, origRestart, origProbe
		probeDaemonForEnsure, getAnyRunningDaemon = origEnsureProbe, origDiscover
	})
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
}

// withRecordingRemote puts the CLI in remote mode against a loopback test
// server and returns a counter of the requests that server received.
func withRecordingRemote(t *testing.T) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	setRemoteEndpoint(&daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")})
	return &hits
}

// withSilentRemote puts the CLI in remote mode against a loopback test
// server that fails the test on any request.
func withSilentRemote(t *testing.T) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Failf(t, "unexpected request to the remote daemon", "%s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	setRemoteEndpoint(&daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")})
}

func writeRemoteConfig(t *testing.T, server string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(os.Getenv("ROBOREV_DATA_DIR"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "config.toml"),
		[]byte("[remote]\nserver = \""+server+"\"\n"), 0o644))
}

func TestRemoteModeFromConfig(t *testing.T) {
	withRemoteState(t)
	writeRemoteConfig(t, "http://daemon-host.example:7474")
	serverAddr = ""
	require.NoError(t, validateServerFlag())
	remote, err := isRemoteMode()
	require.NoError(t, err)
	require.True(t, remote)
	assert.Equal(t, "daemon-host.example:7474", getDaemonEndpoint().Address)

	serverAddr = "http://127.0.0.1:7373" // a loopback flag overrides the config
	require.NoError(t, validateServerFlag())
	remote, err = isRemoteMode()
	require.NoError(t, err)
	assert.False(t, remote)
}

func TestEnsureDaemonRemoteNeverStartsLocal(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	startDaemonForEnsure = func() error { panic("remote mode must not start a local daemon") }
	restartDaemonForEnsure = func() error { panic("remote mode must not restart a local daemon") }

	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{Version: "some-other-version"}, nil
	}
	require.NoError(t, ensureDaemon(), "remote mode skips the version check")

	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, errors.New("connection refused")
	}
	err := ensureDaemon()
	require.ErrorContains(t, err, "remote daemon at http://daemon-host.example:7474 is not reachable")
	require.ErrorContains(t, err, "connection refused")
}

func TestLocalOnlyCommandsFailInRemoteMode(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	err := ensureLocalDaemon("roborev fix")
	require.ErrorContains(t, err, "roborev fix needs a local daemon")
}

func TestAgentHookEndpointIgnoresRemote(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	ep, err := agentHookEndpoint("")
	require.NoError(t, err)
	assert.NotEqual(t, "daemon-host.example:7474", ep.Address)
}

func TestDaemonSubcommandsRefuseRemoteMode(t *testing.T) {
	withRemoteState(t)
	hits := withRecordingRemote(t)
	origStop, origEnsure := daemonStop, daemonEnsure
	t.Cleanup(func() { daemonStop, daemonEnsure = origStop, origEnsure })
	daemonStop = func() error { panic("remote mode must not stop a local daemon") }
	daemonEnsure = func() error { panic("remote mode must not start a local daemon") }

	for _, sub := range []string{"start", "stop", "restart", "status", "run"} {
		t.Run(sub, func(t *testing.T) {
			cmd := daemonCmd()
			cmd.SetArgs([]string{sub})
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			err := cmd.Execute()
			require.ErrorContains(t, err, "roborev daemon "+sub+" needs a local daemon")
		})
	}
	assert.Zero(t, hits.Load(), "no daemon was contacted")
}

func TestInitRemoteModeSkipsRegistration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}
	withRemoteState(t)
	repo := initNoDaemonSetup(t)
	hits := withRecordingRemote(t)
	startDaemonForEnsure = func() error { panic("remote mode must not start a local daemon") }

	output := captureStdout(t, func() {
		cmd := initCmd()
		cmd.SetArgs([]string{})
		require.NoError(t, cmd.Execute())
	})

	assert := assert.New(t)
	assert.FileExists(filepath.Join(repo.HooksDir, "post-commit"))
	assert.Contains(output, "Remote daemon configured (http://"+remoteEndpoint.Address+
		"). Register this repo on the daemon host by running roborev init there.")
	assert.NotContains(output, "Repo registered")
	assert.Zero(hits.Load(), "init must not call the remote daemon")
}

func TestRemapRemoteMode(t *testing.T) {
	withRemoteState(t)
	hits := withRecordingRemote(t)
	chdir(t, t.TempDir()) // not a git repo: local mode would fail here

	cmd := remapCmd()
	cmd.SetArgs([]string{"--quiet"})
	require.NoError(t, cmd.Execute())

	cmd = remapCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	require.ErrorContains(t, cmd.Execute(), "roborev remap needs a local daemon")
	assert.Zero(t, hits.Load(), "remap must not call the remote daemon")
}

func TestEnsureRemoteDaemonReportsRefusal(t *testing.T) {
	withRemoteState(t)
	serverAddr = "http://daemon-host.example:7474"
	require.NoError(t, validateServerFlag())
	probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, &daemon.PingStatusError{StatusCode: http.StatusForbidden, Reason: "tailscale whois failed: no peer"}
	}
	err := ensureDaemon()
	require.EqualError(t, err,
		"remote daemon at http://daemon-host.example:7474 refused the request: tailscale whois failed: no peer")
}

func TestInvalidRemoteConfigOnlyFailsDaemonPaths(t *testing.T) {
	withRemoteState(t)
	writeRemoteConfig(t, "https://daemon-host.example:7474")
	serverAddr = ""
	require.NoError(t, validateServerFlag(), "a bad [remote] server must not fail every command")

	output := captureStdout(t, func() {
		cmd := configGetCmd()
		cmd.SetArgs([]string{"--global", "remote.server"})
		require.NoError(t, cmd.Execute())
	})
	assert.Contains(t, output, "https://daemon-host.example:7474")

	startDaemonForEnsure = func() error { panic("a bad remote config must not start a local daemon") }
	err := ensureDaemon()
	require.ErrorContains(t, err, "invalid [remote] server")
	require.ErrorContains(t, err, "roborev config set --global remote.server")

	// No path falls back to the local daemon on a broken [remote].
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		panic("a broken [remote] must not discover a local daemon")
	}
	require.ErrorContains(t, agentHookEnsureDaemon(), "invalid [remote] server")
	remote, err := isRemoteMode()
	require.ErrorContains(t, err, "invalid [remote] server")
	assert.False(t, remote)
	assert.Equal(t, "127.0.0.1:1", getDaemonEndpoint().Address, "unroutable, never a local daemon")
}

func TestAgentHookRemoteModeNeverManagesLocalDaemon(t *testing.T) {
	setup := func(t *testing.T) {
		withRemoteState(t)
		writeRemoteConfig(t, "http://daemon-host.example:7474")
		serverAddr = ""
		require.NoError(t, validateServerFlag())
		origStop := stopDaemonForRestart
		t.Cleanup(func() { stopDaemonForRestart = origStop })
		startDaemonForEnsure = func() error { panic("remote mode must not start a local daemon") }
		restartDaemonForEnsure = func() error { panic("remote mode must not restart a local daemon") }
		stopDaemonForRestart = func() error { panic("remote mode must not stop a local daemon") }
		probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
			panic("remote mode must not version-check a local daemon")
		}
		probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
			panic("agent hooks must not ping the remote daemon")
		}
	}

	t.Run("no local daemon", func(t *testing.T) {
		setup(t)
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
		require.ErrorIs(t, agentHookEnsureDaemon(), ErrDaemonNotRunning)
	})

	t.Run("stale local daemon", func(t *testing.T) {
		setup(t)
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{PID: 42, Address: "127.0.0.1:7373", Version: "stale-version"}, nil
		}
		require.NoError(t, agentHookEnsureDaemon())
	})
}

func TestDaemonRunArgsPassLocalServerThrough(t *testing.T) {
	withRemoteState(t)
	writeRemoteConfig(t, "http://daemon-host.example:7474")

	serverAddr = "127.0.0.1:7373"
	require.NoError(t, validateServerFlag())
	assert.Equal(t, []string{"--server", "127.0.0.1:7373", "daemon", "run"}, daemonRunArgs())

	serverAddr = ""
	require.NoError(t, validateServerFlag())
	assert.Equal(t, []string{"daemon", "run"}, daemonRunArgs())
}

func TestBrokenRemoteConfigNeverReachesLocalDaemon(t *testing.T) {
	var hits atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(local.Close)
	brokenRemote := func(t *testing.T) {
		writeRemoteConfig(t, "https://daemon-host.example:7474")
		serverAddr = ""
		require.NoError(t, validateServerFlag())
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{Address: strings.TrimPrefix(local.URL, "http://")}, nil
		}
	}

	t.Run("init --no-daemon", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("skipping on Windows due to shell script stubs")
		}
		withRemoteState(t)
		initNoDaemonSetup(t)
		brokenRemote(t)
		var err error
		output := captureStdout(t, func() {
			cmd := initCmd()
			cmd.SetArgs([]string{"--no-daemon"})
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			err = cmd.Execute()
		})
		require.ErrorContains(t, err, "invalid [remote] server")
		assert.NotContains(t, output, "Repo registered")
	})

	t.Run("status", func(t *testing.T) {
		withRemoteState(t)
		brokenRemote(t)
		origEnsure := statusEnsureDaemon
		t.Cleanup(func() { statusEnsureDaemon = origEnsure })
		statusEnsureDaemon = func() error { panic("a broken [remote] must not check a local daemon") }
		cmd := statusCmd()
		cmd.SetArgs([]string{})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		require.ErrorContains(t, cmd.Execute(), "invalid [remote] server")
	})

	t.Run("quickstart", func(t *testing.T) {
		withRemoteState(t)
		brokenRemote(t)
		up, err := daemonReachable()
		require.ErrorContains(t, err, "invalid [remote] server")
		assert.False(t, up)
		check := checkDaemon(up, err)
		assert.Contains(t, check.Details, "invalid [remote] server")
		assert.Equal(t, "roborev config set --global remote.server <url>", check.FixCommand)
	})

	assert.Zero(t, hits.Load(), "no request reached the local daemon")
}

func TestLocalOnlyRefusalIsNotWrapped(t *testing.T) {
	withRemoteState(t)
	withSilentRemote(t)
	for name, cmd := range map[string]func() *cobra.Command{
		"roborev sync":   syncNowCmd,
		"roborev export": exportReviewsCmd,
	} {
		t.Run(name, func(t *testing.T) {
			c := cmd()
			c.SetArgs([]string{})
			c.SilenceUsage = true
			c.SilenceErrors = true
			err := c.Execute()
			require.ErrorContains(t, err, name+" needs a local daemon")
			assert.NotContains(t, err.Error(), "daemon not running")
		})
	}
}

func TestUIRefusesRemoteMode(t *testing.T) {
	withRemoteState(t)
	withUICommandDependencies(t,
		func() error { panic("remote mode must not probe for the UI") },
		func() (*daemon.RuntimeInfo, error) { panic("remote mode must not discover a local UI") },
		func(string) error { panic("remote mode must not open a browser") },
	)
	withSilentRemote(t)
	cmd := uiCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	require.ErrorContains(t, cmd.Execute(), "roborev ui needs a local daemon")
}

func TestReviewDirtyRemoteSendsNothing(t *testing.T) {
	withRemoteState(t)
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "initial\n", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, "file.txt"), []byte("changed\n"), 0o600))
	withSilentRemote(t)

	_, _, err := executeReviewCmd("--repo", repo.Dir, "--dirty", "--agent", "test", "--quiet")
	require.ErrorContains(t, err, "roborev review --dirty needs a local daemon")
}

func TestTUIAddrSelectsEndpoint(t *testing.T) {
	runTUI := func(addr string) error {
		cmd := tuiCmd()
		cmd.SetArgs([]string{"--addr", addr})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return cmd.Execute()
	}

	t.Run("remote addr", func(t *testing.T) {
		withRemoteState(t)
		serverAddr = ""
		require.NoError(t, validateServerFlag())
		var probed daemon.DaemonEndpoint
		probeRemoteDaemon = func(ep daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
			probed = ep
			return nil, errors.New("remote down")
		}
		err := runTUI("http://daemon-host.example:7474")
		require.ErrorContains(t, err, "remote daemon at http://daemon-host.example:7474 is not reachable: remote down")
		assert.Equal(t, "daemon-host.example:7474", probed.Address)
	})

	t.Run("local addr overrides remote config", func(t *testing.T) {
		withRemoteState(t)
		writeRemoteConfig(t, "http://daemon-host.example:7474")
		serverAddr = ""
		require.NoError(t, validateServerFlag())
		probeRemoteDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
			panic("a local --addr must not probe the configured remote")
		}
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
		// Stop ensureDaemon at its local probe, so the test neither starts a
		// daemon nor opens the TUI.
		probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
			return nil, daemon.ErrDaemonAccessDenied
		}
		err := runTUI("http://127.0.0.1:1")
		require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
		remote, err := isRemoteMode()
		require.NoError(t, err)
		assert.False(t, remote)
	})
}
