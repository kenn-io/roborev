package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

func TestGetDaemonEndpointAvoidsDefaultDaemonPortInTests(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	if !isGoTestBinaryPath(exe) {
		t.Skipf("expected go test binary path, got %q", exe)
	}

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGetAnyRunningDaemon := getAnyRunningDaemon
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGetAnyRunningDaemon
	})

	got := getDaemonEndpoint()
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "127.0.0.1:1", got.Address)
}

func TestGetDaemonEndpointIgnoresCachedDefaultFromEmptyServerFlagInTests(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	if !isGoTestBinaryPath(exe) {
		t.Skipf("expected go test binary path, got %q", exe)
	}

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGetAnyRunningDaemon := getAnyRunningDaemon
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGetAnyRunningDaemon
	})

	require.NoError(t, validateServerFlag())

	got := getDaemonEndpoint()
	assert.Equal(t, "tcp", got.Network)
	assert.Equal(t, "127.0.0.1:1", got.Address)
}

func TestEnsureDaemonPrefersLiveDaemonVersionOverRuntimeMetadata(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			_ = json.NewEncoder(w).Encode(daemon.PingInfo{
				OK:      true,
				Service: "roborev",
				Version: "v-other-daemon",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	origGetAnyRunningDaemon := getAnyRunningDaemon
	origRestartDaemon := restartDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			PID:     1234,
			Address: strings.TrimPrefix(server.URL, "http://"),
			Version: version.Version,
		}, nil
	}
	restartCalls := 0
	restartDaemonForEnsure = func() error {
		restartCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemon = origGetAnyRunningDaemon
		restartDaemonForEnsure = origRestartDaemon
	})

	if err := ensureDaemon(); err != nil {
		require.NoError(t, err, "ensureDaemon returned error: %v")
	}
	assert.Equal(t, 1, restartCalls)
}

func TestEnsureDaemonRestartsWhenLiveProbeFailsDespiteRuntimeVersion(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origGetAnyRunningDaemon := getAnyRunningDaemon
	origRestartDaemon := restartDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			PID:     1234,
			Address: "127.0.0.1:1",
			Version: version.Version,
		}, nil
	}
	restartCalls := 0
	restartDaemonForEnsure = func() error {
		restartCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemon = origGetAnyRunningDaemon
		restartDaemonForEnsure = origRestartDaemon
	})

	if err := ensureDaemon(); err != nil {
		require.NoError(t, err, "ensureDaemon returned error: %v")
	}
	assert.Equal(t, 1, restartCalls)
}
