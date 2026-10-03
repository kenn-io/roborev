package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/secretref"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
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

func TestSameRuntimeEndpointAddressMatchesLocalhostAliases(t *testing.T) {
	for _, tc := range []struct {
		name            string
		runtimeAddress  string
		selectedAddress string
		want            bool
	}{
		{name: "exact", runtimeAddress: "127.0.0.1:7373", selectedAddress: "127.0.0.1:7373", want: true},
		{name: "localhost to IPv4", runtimeAddress: "localhost:7373", selectedAddress: "127.0.0.1:7373", want: true},
		{name: "IPv6 to localhost", runtimeAddress: "[::1]:7373", selectedAddress: "localhost:7373", want: true},
		{name: "different port", runtimeAddress: "127.0.0.1:7373", selectedAddress: "localhost:7374"},
		{name: "different loopback IP", runtimeAddress: "127.0.0.2:7373", selectedAddress: "localhost:7373"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, daemon.SameRuntimeEndpointAddress(tc.runtimeAddress, tc.selectedAddress))
		})
	}
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
	require.True(t, daemon.IsDaemonAccessError(err))
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
	require.True(t, daemon.IsDaemonAccessError(err))
	assert.Zero(t, cleanupCalls)
	assert.Zero(t, startCalls)
}

func TestEnsureDaemonColdStartsWhenAuthKeyRequiresTLSAndNoRuntimeExists(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origOccupied := tcpEndpointOccupiedForEnsure
	origCleanup := cleanupZombieDaemons
	origStart := startDaemonForEnsure
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	probeDaemonForEnsure = daemon.ProbeDaemon
	occupiedCalls, cleanupCalls, startCalls := 0, 0, 0
	tcpEndpointOccupiedForEnsure = func(endpoint daemon.DaemonEndpoint) bool {
		occupiedCalls++
		assert.Equal(t, "tcp", endpoint.Network)
		return false
	}
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanupCalls++; return 0 }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		tcpEndpointOccupiedForEnsure = origOccupied
		cleanupZombieDaemons = origCleanup
		startDaemonForEnsure = origStart
	})

	require.NoError(t, ensureDaemon())
	assert.Equal(t, 1, occupiedCalls)
	assert.Equal(t, 1, cleanupCalls)
	assert.Equal(t, 1, startCalls)
}

func TestEnsureDaemonDoesNotColdStartWhenUnverifiedEndpointIsOccupied(t *testing.T) {
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGet := getAnyRunningDaemon
	origProbe := probeDaemonForEnsure
	origOccupied := tcpEndpointOccupiedForEnsure
	origCleanup := cleanupZombieDaemons
	origStart := startDaemonForEnsure
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return nil, daemon.ErrPlaintextAuthTransport
	}
	occupiedCalls, cleanupCalls, startCalls := 0, 0, 0
	tcpEndpointOccupiedForEnsure = func(endpoint daemon.DaemonEndpoint) bool {
		occupiedCalls++
		assert.Equal(t, "tcp", endpoint.Network)
		return true
	}
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanupCalls++; return 0 }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGet
		probeDaemonForEnsure = origProbe
		tcpEndpointOccupiedForEnsure = origOccupied
		cleanupZombieDaemons = origCleanup
		startDaemonForEnsure = origStart
	})

	err := ensureDaemon()
	require.ErrorIs(t, err, daemon.ErrDaemonAccessDenied)
	assert.Equal(t, 1, occupiedCalls)
	assert.Zero(t, cleanupCalls)
	assert.Zero(t, startCalls)
}

func TestEnsureDaemonWaitsForConcurrentDaemonStartBeforeRefusingOccupiedEndpoint(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))

	origServerAddr := serverAddr
	origParsed := parsedServerEndpoint
	origGet := getAnyRunningDaemon
	origGetForStart := getAnyRunningDaemonForStart
	origListRuntimes := listAllRuntimes
	origProbe := probeDaemonForEnsure
	origOccupied := tcpEndpointOccupiedForEnsure
	origAcquireStartLock := acquireStartLockForEnsure
	origCleanup := cleanupZombieDaemons
	origStart := startDaemonForEnsure
	serverAddr = ""
	parsedServerEndpoint = nil
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	occupiedEndpoint := getDaemonEndpoint()
	published := make(chan struct{})
	info := &daemon.RuntimeInfo{
		PID:        os.Getpid(),
		Network:    "tcp",
		Address:    occupiedEndpoint.Address,
		Service:    "roborev",
		Version:    version.Version,
		TLSCertPEM: "test-pinned-certificate",
	}
	unrelated := &daemon.RuntimeInfo{
		PID:        os.Getpid(),
		Network:    "tcp",
		Address:    "127.0.0.1:7374",
		Service:    "roborev",
		Version:    version.Version,
		TLSCertPEM: "unrelated-pinned-certificate",
	}
	assert.Equal(info.Address, getDaemonEndpoint().Address)
	assert.False(info.HasStaleProcess())
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
		select {
		case <-published:
			return unrelated, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	listCalls := 0
	listBeforePublication := false
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		listCalls++
		select {
		case <-published:
			return []*daemon.RuntimeInfo{unrelated, info}, nil
		default:
			listBeforePublication = true
			return nil, nil
		}
	}
	probeCalls, cleanupCalls, startCalls := 0, 0, 0
	probeDaemonForEnsure = func(endpoint daemon.DaemonEndpoint, _ time.Duration) (*daemon.PingInfo, error) {
		probeCalls++
		if endpoint.TLSCertPEM == "" {
			return nil, daemon.ErrPlaintextAuthTransport
		}
		if endpoint.Address != info.Address {
			return nil, daemon.ErrPlaintextAuthTransport
		}
		return &daemon.PingInfo{PID: info.PID, Version: version.Version}, nil
	}
	occupied := make(chan struct{}, 1)
	tcpEndpointOccupiedForEnsure = func(endpoint daemon.DaemonEndpoint) bool {
		assert.Equal("tcp", endpoint.Network)
		occupied <- struct{}{}
		return true
	}
	lockRequested := make(chan struct{}, 1)
	acquireStartLockForEnsure = func(ctx context.Context) (func(), error) {
		lockRequested <- struct{}{}
		return origAcquireStartLock(ctx)
	}
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanupCalls++; return 0 }
	startDaemonForEnsure = func() error { startCalls++; return nil }
	t.Cleanup(func() {
		serverAddr = origServerAddr
		parsedServerEndpoint = origParsed
		getAnyRunningDaemon = origGet
		getAnyRunningDaemonForStart = origGetForStart
		listAllRuntimes = origListRuntimes
		probeDaemonForEnsure = origProbe
		tcpEndpointOccupiedForEnsure = origOccupied
		acquireStartLockForEnsure = origAcquireStartLock
		cleanupZombieDaemons = origCleanup
		startDaemonForEnsure = origStart
	})

	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonStartLockHelper$")
	cmd.Env = append(
		os.Environ(),
		"ROBOREV_DAEMON_START_LOCK_HELPER=1",
		"ROBOREV_DAEMON_START_LOCK_DIR="+os.Getenv("ROBOREV_DATA_DIR"),
	)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)

	done := make(chan struct{})
	var ensureErr error
	go func() {
		ensureErr = ensureDaemon()
		close(done)
	}()
	<-occupied

	returnedBeforeLock := false
	select {
	case <-lockRequested:
	case <-done:
		returnedBeforeLock = true
	}
	require.False(t, returnedBeforeLock, "ensure returned before waiting for the concurrent daemon startup lock")

	close(published)
	require.NoError(t, stdin.Close())
	<-done
	require.NoError(t, ensureErr)
	assert.Equal(1, listCalls)
	assert.False(listBeforePublication)
	assert.Equal(2, probeCalls)
	assert.Zero(cleanupCalls)
	assert.Zero(startCalls)
}

func TestDaemonStartLockHelper(t *testing.T) {
	if os.Getenv("ROBOREV_DAEMON_START_LOCK_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	dataDir := os.Getenv("ROBOREV_DAEMON_START_LOCK_DIR")
	require.NotEmpty(t, dataDir)
	t.Setenv("ROBOREV_DATA_DIR", dataDir)

	release, err := daemon.RuntimeStore().AcquireStartLock(context.Background())
	require.NoError(t, err)
	defer release()

	_, err = io.WriteString(os.Stdout, "locked\n")
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, os.Stdin)
	assert.NoError(t, err)
}

func TestTCPEndpointOccupancyCheckConnectsWithoutSendingData(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	endpoint := daemon.DaemonEndpoint{Network: "tcp", Address: listener.Addr().String()}
	require.True(t, isTCPEndpointOccupied(endpoint))
	conn := <-accepted // The test waits for the local TCP connect to finish.
	defer conn.Close()
	// The accepted TCP socket must close without receiving an HTTP request.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	buffer := make([]byte, 1)
	count, err := conn.Read(buffer)
	assert.Zero(t, count)
	assert.Equal(t, io.EOF, err)
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
		if discoveryCalls <= 2 {
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

func TestStartDaemonReportsStartupProgress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		readyAfter  time.Duration
		startupLog  string
		wantMessage string
		wantTimeout bool
	}{
		{name: "fast startup", readyAfter: time.Second},
		{
			name: "database initialization", readyAfter: 30 * time.Second,
			startupLog:  "Opening database\nRestoring archived reviews\n",
			wantMessage: "Restoring archived reviews",
		},
		{
			name: "startup failure", readyAfter: 3 * time.Minute,
			startupLog:  "Opening database\nError: invalid daemon configuration\n",
			wantMessage: "Error: invalid daemon configuration", wantTimeout: true,
		},
		{name: "timeout without new logs", readyAfter: 3 * time.Minute, wantTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := testenv.SetDataDir(t)
			logPath := filepath.Join(dataDir, "logs", "daemon.stderr.log")
			require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
			require.NoError(t, os.WriteFile(logPath, []byte("Error: previous startup failure\n"), 0o600))
			origGet, origStart, origOut := getAnyRunningDaemonForStart, startDaemonDetached, lifecycleOut
			t.Cleanup(func() {
				getAnyRunningDaemonForStart, startDaemonDetached, lifecycleOut = origGet, origStart, origOut
			})

			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				var progress bytes.Buffer
				lifecycleOut = &progress
				var readyAt time.Time
				getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) {
					if readyAt.IsZero() || time.Now().Before(readyAt) {
						return nil, os.ErrNotExist
					}
					return &daemon.RuntimeInfo{PID: os.Getpid()}, nil
				}
				spawnCalls := 0
				startDaemonDetached = func(_ context.Context, opts detachedDaemonOptions) error {
					spawnCalls++
					// Database initialization may consume SQLite's 30-second busy
					// wait before the daemon can publish its runtime.
					readyAt = time.Now().Add(tc.readyAfter)
					_, err := io.WriteString(opts.Stderr, tc.startupLog)
					return err
				}

				err := startDaemon()
				if tc.wantTimeout {
					require.ErrorIs(t, err, context.DeadlineExceeded)
					assert.Contains(err.Error(), "2m0s")
					assert.Contains(err.Error(), logPath)
					assert.NotContains(err.Error(), "previous startup failure")
					if tc.wantMessage != "" {
						assert.Contains(err.Error(), tc.wantMessage)
					}
				} else {
					require.NoError(t, err)
				}
				assert.Equal(1, spawnCalls)
				if tc.readyAfter == time.Second {
					assert.Empty(progress.String())
				} else {
					assert.Contains(progress.String(), "Still waiting for daemon startup")
					assert.Contains(progress.String(), logPath)
					assert.NotContains(progress.String(), "previous startup failure")
					if tc.wantMessage != "" {
						assert.Contains(progress.String(), tc.wantMessage)
					}
				}
			})
		})
	}
}

func TestDaemonSearchOpensDerivedSidecarWithoutEmbeddingsAndClosesIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	db, err := storage.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	search, err := newDaemonSearch(t.Context(), db, dbPath, config.DefaultConfig())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(filepath.Dir(dbPath), "reviews.search.db"), search.path)
	assert.FileExists(t, search.path)

	result, err := search.service.Search(t.Context(), searchindex.SearchParams{
		Query: "needle", Mode: searchindex.ModeLexical, Limit: 20,
	})
	require.NoError(t, err)
	assert.Empty(t, result.Hits)
	assert.Equal(t, searchindex.ModeLexical, result.Mode)

	require.NoError(t, search.Close())
	_, _, err = search.index.ActiveGeneration(t.Context())
	require.Error(t, err)
}

func TestDaemonSearchClosesSidecarOnConstructionFailures(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*config.Config)
		wantError  string
		notInError string
	}{
		{
			name: "partial config",
			configure: func(cfg *config.Config) {
				cfg.Search.Embeddings = &embedconfig.Embedder{BaseURL: "https://embeddings.example"}
			},
			wantError: "search.embeddings: embed base_url, model, and positive dims",
		},
		{
			name: "invalid embedding client",
			configure: func(cfg *config.Config) {
				cfg.Search.Embeddings = &embedconfig.Embedder{
					BaseURL: "file:///tmp/provider", Model: "model", Dims: 2,
				}
			},
			wantError: "embed endpoint must use http or https",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "reviews")
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			originalClose := closeDaemonSearchIndex
			closeCalls := 0
			closeDaemonSearchIndex = func(index *searchindex.Index) error {
				closeCalls++
				return originalClose(index)
			}
			t.Cleanup(func() { closeDaemonSearchIndex = originalClose })

			cfg := config.DefaultConfig()
			tc.configure(cfg)
			_, err = newDaemonSearch(t.Context(), db, dbPath, cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError)
			if tc.notInError != "" {
				assert.NotContains(t, err.Error(), tc.notInError)
			}
			assert.Equal(t, 1, closeCalls)
		})
	}
}

type fakeDaemonLifecycle struct {
	startErr error
	stopErr  error
	starts   int
	stops    int
}

func (f *fakeDaemonLifecycle) Start(context.Context) error {
	f.starts++
	return f.startErr
}

func (f *fakeDaemonLifecycle) Stop() error {
	f.stops++
	return f.stopErr
}

type fakeSearchCloser struct {
	err   error
	calls int
}

func (f *fakeSearchCloser) Close() error {
	f.calls++
	return f.err
}

func TestRunDaemonWithSearchClosesSidecarOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startErr error
		stopErr  error
		closeErr error
	}{
		{name: "normal shutdown"},
		{name: "startup failure", startErr: errors.New("listen failed")},
		{name: "shutdown failure", stopErr: errors.New("drain failed")},
		{name: "sidecar close failure", closeErr: errors.New("close failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeDaemonLifecycle{startErr: tc.startErr, stopErr: tc.stopErr}
			search := &fakeSearchCloser{err: tc.closeErr}

			err := runDaemonWithSearch(t.Context(), server, search)
			assert.Equal(t, 1, server.starts)
			assert.Equal(t, 1, server.stops)
			assert.Equal(t, 1, search.calls)
			for _, expected := range []error{tc.startErr, tc.stopErr, tc.closeErr} {
				if expected != nil {
					require.ErrorIs(t, err, expected)
				}
			}
		})
	}
}

func TestDaemonSearchMissingCredentialStartsLexical(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(401) }))
	defer provider.Close()
	t.Setenv("ROBOREV_TEST_EMBEDDING_KEY", "")
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	db, err := storage.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("test-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJobResult(job.ID, "test", "prompt", storage.ReviewCompletion{StructuredOutput: []byte(`{"schema_version":1,"summary":"needle review","findings":[]}`), Verdict: storage.VerdictPass}))
	cfg := config.DefaultConfig()
	cfg.Search.Embeddings = &embedconfig.Embedder{BaseURL: provider.URL, Model: "test", Dims: 2, APIKey: secretref.Ref{Env: "ROBOREV_TEST_EMBEDDING_KEY"}, TrustPrivateNetwork: true}
	search, err := newDaemonSearch(t.Context(), db, dbPath, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, search.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- search.reconciler.Run(ctx) }()
	require.Eventually(t, func() bool { return search.reconciler.Health().MirrorComplete }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	health := search.reconciler.Health()
	assert.Equal(t, "missing", health.Credential)
	assert.Equal(t, "env:ROBOREV_TEST_EMBEDDING_KEY", health.CredentialSource)
	result, err := search.service.Search(t.Context(), searchindex.SearchParams{Query: "needle"})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, job.ID, result.Hits[0].JobID)
	assert.Equal(t, searchindex.ModeLexical, result.Mode)
	assert.True(t, result.Degraded)
	assert.Contains(t, result.DegradedReason, "no embedding API key")
	assert.Contains(t, result.DegradedReason, "ROBOREV_TEST_EMBEDDING_KEY")
	assert.True(t, result.Coverage.EmbeddingsConfigured)
	assert.Equal(t, searchindex.VectorUnavailable, result.Coverage.VectorState)
	for _, mode := range []searchindex.SearchMode{searchindex.ModeSemantic, searchindex.ModeHybrid} {
		_, err = search.service.Search(t.Context(), searchindex.SearchParams{Query: "needle", Mode: mode})
		var modeErr *searchindex.ModeError
		require.ErrorAs(t, err, &modeErr)
		assert.Equal(t, 503, modeErr.Status)
		assert.Equal(t, result.DegradedReason, modeErr.Reason)
	}
	assert.Zero(t, requests.Load())
	assert.NotContains(t, logs.String(), "embedding API key")
}

func TestDaemonSearchCredentialFiles(t *testing.T) {
	for _, tc := range []struct {
		name, contents, credential, reason string
		mode                               os.FileMode
	}{
		{"private", "example-key\n", "ok", "", 0o600},
		{"empty", "\n", "missing", "file is empty", 0o600},
		{"insecure", "example-key", "missing", "must be private", 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tc.name == "insecure" {
				t.Skip("Unix permissions")
			}
			path := filepath.Join(t.TempDir(), "embedding.key")
			file, err := safefileio.CreatePrivateFile(path)
			require.NoError(t, err)
			_, err = file.WriteString(tc.contents)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			if tc.mode != 0o600 {
				require.NoError(t, os.Chmod(path, tc.mode))
			}
			dbPath := filepath.Join(t.TempDir(), "reviews.db")
			db, err := storage.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			cfg := config.DefaultConfig()
			cfg.Search.Embeddings = &embedconfig.Embedder{BaseURL: "https://api.example.test/v1", Model: "test", Dims: 2, APIKey: secretref.Ref{File: path}}
			search, err := newDaemonSearch(t.Context(), db, dbPath, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, search.Close()) })
			health := search.reconciler.Health()
			assert.Equal(t, tc.credential, health.Credential)
			assert.Equal(t, "file:"+path, health.CredentialSource)
			if tc.reason != "" {
				assert.Contains(t, health.CredentialReason, tc.reason)
			} else {
				assert.Empty(t, health.CredentialReason)
			}
			assert.NotContains(t, health.CredentialReason, "example-key")
		})
	}
}
