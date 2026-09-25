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
	origServer, origParsed, origRemote := serverAddr, parsedServerEndpoint, remoteEndpoint
	origStart, origRestart, origProbe := startDaemonForEnsure, restartDaemonForEnsure, probeRemoteDaemon
	t.Cleanup(func() {
		serverAddr, parsedServerEndpoint, remoteEndpoint = origServer, origParsed, origRemote
		startDaemonForEnsure, restartDaemonForEnsure, probeRemoteDaemon = origStart, origRestart, origProbe
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
	remoteEndpoint = &daemon.DaemonEndpoint{Network: "tcp", Address: strings.TrimPrefix(ts.URL, "http://")}
	return &hits
}

func TestRemoteModeFromConfig(t *testing.T) {
	withRemoteState(t)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), "config.toml"),
		[]byte("[remote]\nserver = \"http://daemon-host.example:7474\"\n"), 0o644))
	serverAddr = ""
	require.NoError(t, validateServerFlag())
	require.True(t, isRemoteMode())
	assert.Equal(t, "daemon-host.example:7474", getDaemonEndpoint().Address)

	serverAddr = "http://127.0.0.1:7373" // a loopback flag overrides the config
	require.NoError(t, validateServerFlag())
	assert.False(t, isRemoteMode())
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
