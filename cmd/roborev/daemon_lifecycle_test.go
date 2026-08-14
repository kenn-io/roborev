package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testenv"
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
	origRetryDelay := ensureProbeRetryDelay
	ensureProbeRetryDelay = time.Millisecond
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
		ensureProbeRetryDelay = origRetryDelay
	})

	if err := ensureDaemon(); err != nil {
		require.NoError(t, err, "ensureDaemon returned error: %v")
	}
	assert.Equal(t, 1, restartCalls)
}

func TestEnsureDaemonCleansZombiesBeforeColdStart(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGetAnyRunningDaemon := getAnyRunningDaemon
	origCleanupZombieDaemons := cleanupZombieDaemons
	origStartDaemon := startDaemonForEnsure
	origRetryDelay := ensureProbeRetryDelay
	serverAddr = ""
	parsedServerEndpoint = nil
	ensureProbeRetryDelay = time.Millisecond
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}

	var calls []string
	cleanupZombieDaemons = func(target daemon.DaemonEndpoint) int {
		calls = append(calls, "cleanup:"+target.Address)
		return 1
	}
	startDaemonForEnsure = func() error {
		calls = append(calls, "start")
		return nil
	}
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGetAnyRunningDaemon
		cleanupZombieDaemons = origCleanupZombieDaemons
		startDaemonForEnsure = origStartDaemon
		ensureProbeRetryDelay = origRetryDelay
	})

	require.NoError(t, ensureDaemon())
	assert.Equal(t, []string{"cleanup:127.0.0.1:1", "start"}, calls)
}

func TestEnsureDaemonDoesNotRecoverFromAccessDeniedDiscovery(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origCleanup := cleanupZombieDaemons
	origRestart := restartDaemonForEnsure
	origStart := startDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, daemon.ErrDaemonAccessDenied
	}
	probeCalls, cleanupCalls, restartCalls, startCalls := 0, 0, 0, 0
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		probeCalls++
		return nil, nil
	}
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanupCalls++; return 0 }
	restartDaemonForEnsure = func() error { restartCalls++; return nil }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		cleanupZombieDaemons = origCleanup
		restartDaemonForEnsure = origRestart
		startDaemonForEnsure = origStart
	})

	err := ensureDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Zero(t, probeCalls)
	assert.Zero(t, cleanupCalls)
	assert.Zero(t, restartCalls)
	assert.Zero(t, startCalls)
}

func TestEnsureDaemonDoesNotRestartAfterAccessDeniedVersionProbe(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origRestart := restartDaemonForEnsure
	origStart := startDaemonForEnsure
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{Network: "tcp", Address: "127.0.0.1:7373"}, nil
	}
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EPERM}
	}
	restartCalls, startCalls := 0, 0
	restartDaemonForEnsure = func() error { restartCalls++; return nil }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		restartDaemonForEnsure = origRestart
		startDaemonForEnsure = origStart
	})

	err := ensureDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Zero(t, restartCalls)
	assert.Zero(t, startCalls)
}

func TestEnsureDaemonDoesNotColdStartAfterAccessDeniedDefaultProbe(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origCleanup := cleanupZombieDaemons
	origStart := startDaemonForEnsure
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}
	}
	cleanupCalls, startCalls := 0, 0
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanupCalls++; return 0 }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		cleanupZombieDaemons = origCleanup
		startDaemonForEnsure = origStart
	})

	err := ensureDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Zero(t, cleanupCalls)
	assert.Zero(t, startCalls)
}

func TestStartDaemonUsesAlternateAwareDiscoveryInsideStartLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are not supported on Windows")
	}
	testenv.SetDataDir(t)

	primary := daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}
	alternate := daemon.DaemonEndpoint{
		Network: "unix",
		Address: filepath.Join(t.TempDir(), "daemon.sock"),
	}
	require.NoError(t, daemon.WriteRuntime(primary, &alternate, version.Version, nil))

	origGet := getAnyRunningDaemonForStart
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			PID:     os.Getpid(),
			Network: alternate.Network,
			Address: alternate.Address,
			Version: version.Version,
		}, nil
	}
	t.Cleanup(func() { getAnyRunningDaemonForStart = origGet })

	require.NoError(t, startDaemon())
}

func TestRestartDaemonDoesNotStartAfterGracefulStopFailure(t *testing.T) {
	origStop := stopDaemonForRestart
	origStart := startDaemonAfterRestart
	t.Cleanup(func() {
		stopDaemonForRestart = origStop
		startDaemonAfterRestart = origStart
	})

	stopErr := errors.New("graceful shutdown unavailable")
	stopDaemonForRestart = func() error { return stopErr }
	startCalls := 0
	startDaemonAfterRestart = func() error {
		startCalls++
		return nil
	}

	err := restartDaemon()

	require.ErrorIs(t, err, stopErr)
	assert.Zero(t, startCalls)
}

func TestStartDaemonDoesNotSpawnAfterAccessDeniedDiscoveryInsideStartLock(t *testing.T) {
	testenv.SetDataDir(t)

	origGet := getAnyRunningDaemonForStart
	origStart := startDaemonDetached
	spawnCalls := 0
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		return nil, daemon.ErrDaemonAccessDenied
	}
	startDaemonDetached = func(context.Context, detachedDaemonOptions) error {
		spawnCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemonForStart = origGet
		startDaemonDetached = origStart
	})

	err := startDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Zero(t, spawnCalls)
}

func TestStartDaemonUsesAlternateAwareDiscoveryWhileWaiting(t *testing.T) {
	testenv.SetDataDir(t)

	origGet := getAnyRunningDaemonForStart
	origStart := startDaemonDetached
	discoveryCalls := 0
	spawnCalls := 0
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		discoveryCalls++
		if discoveryCalls == 1 {
			return nil, os.ErrNotExist
		}
		return &daemon.RuntimeInfo{
			PID:     os.Getpid(),
			Network: "unix",
			Address: filepath.Join(t.TempDir(), "daemon.sock"),
			Version: version.Version,
		}, nil
	}
	startDaemonDetached = func(context.Context, detachedDaemonOptions) error {
		spawnCalls++
		return nil
	}
	t.Cleanup(func() {
		getAnyRunningDaemonForStart = origGet
		startDaemonDetached = origStart
	})

	require.NoError(t, startDaemon())
	assert.Equal(t, 1, spawnCalls)
}

func TestDiscoverDaemonForStartHonorsCanceledContext(t *testing.T) {
	testenv.SetDataDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ready, err := discoverDaemonForStart(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, ready)
}
