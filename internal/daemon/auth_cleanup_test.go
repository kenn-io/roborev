package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
)

func TestAuthShutdownDoesNotRemoveDeniedRuntimeWithoutPID(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	path := filepath.Join(config.DataDir(), "legacy-runtime.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"pid":0}`), 0o600))
	info := &RuntimeInfo{Network: ep.Network, Address: ep.Address, SourcePath: path}
	require.ErrorIs(t, KillDaemon(info), ErrDaemonAccessDenied)
	assert.FileExists(t, path)
}

func TestAuthShutdownReportsDeniedRequest(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server := httptest.NewServer(newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015").httpServer.Handler)
	defer server.Close()
	ep := authEndpoint(t, server.URL)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := requestGracefulDaemonShutdown(ctx, ep, func() bool { return false })
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
}

func TestAuthShutdownUsesVerifiedUnkeyedRequestWithoutSendingKey(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))

	addr, mux := startMockDaemon(t)
	var stopped atomic.Bool
	seenAuth := make(chan string, 2)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		seenAuth <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: daemonServiceName, PID: os.Getpid()})
	})
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		seenAuth <- r.Header.Get("Authorization")
		stopped.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	require.NoError(t, os.WriteFile(runtimePath, []byte("{}"), 0o600))
	info := &RuntimeInfo{
		PID:             os.Getpid(),
		Network:         "tcp",
		Address:         addr,
		Service:         daemonServiceName,
		SourcePath:      runtimePath,
		processIdentity: currentProcessIdentity(t),
	}
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })

	originalAlive := isProcessAlive
	isProcessAlive = func(pid int) bool {
		return pid == info.PID && !stopped.Load()
	}
	t.Cleanup(func() { isProcessAlive = originalAlive })

	require.NoError(t, KillDaemon(info))
	assert.Equal(t, []string{"", ""}, []string{<-seenAuth, <-seenAuth})
	assert.NoFileExists(t, runtimePath)
}

func TestAuthShutdownRejectsLegacyRuntimeWithoutProcessIdentity(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))

	addr, mux := startMockDaemon(t)
	var shutdownCalls atomic.Int32
	var shutdownAccepted atomic.Bool
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		shutdownCalls.Add(1)
		shutdownAccepted.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	path := writeLegacyRuntimeFile(t, dataDir, "daemon.json", os.Getpid(), addr)
	runtimes := listLegacyRuntimes()
	require.Len(t, runtimes, 1)
	info := runtimes[0]
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })
	originalAlive := isProcessAlive
	isProcessAlive = func(pid int) bool {
		return pid == info.PID && !shutdownAccepted.Load()
	}
	t.Cleanup(func() { isProcessAlive = originalAlive })

	require.ErrorIs(t, KillDaemon(info), ErrDaemonAccessDenied)
	assert.Zero(t, shutdownCalls.Load(), "legacy runtime without process identity must not send shutdown")
	assert.FileExists(t, path, "denied legacy runtime must remain available for manual recovery")
}

func TestUnauthenticatedShutdownRequiresRecognizedLocalProcess(t *testing.T) {
	info := &RuntimeInfo{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:7373", Service: daemonServiceName}
	mockIdentifyProcess(t, func(int) processIdentity { return processUnknown })

	require.ErrorIs(t, requestUnauthenticatedDaemonShutdown(t.Context(), info, info.Endpoint(), func() bool { return false }), ErrDaemonAccessDenied)
}

func TestLegacyUnauthenticatedShutdownRejectsMismatchedProcessIdentity(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessIdentityHelper$")
	cmd.Env = append(os.Environ(), "ROBOREV_PROCESS_IDENTITY_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	otherIdentity, ok := kitdaemon.ReadProcessIdentity(cmd.Process.Pid)
	require.True(t, ok)
	assert.Equal(t, kitdaemon.ProcessIdentityMismatch, kitdaemon.CompareProcessIdentity(os.Getpid(), otherIdentity))

	info := &RuntimeInfo{
		PID:             os.Getpid(),
		Network:         "tcp",
		Address:         "127.0.0.1:7373",
		processIdentity: otherIdentity,
		legacyRecord:    true,
	}
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })

	require.ErrorIs(t, requestUnauthenticatedDaemonShutdown(t.Context(), info, info.Endpoint(), func() bool { return false }), ErrDaemonAccessDenied)
}

func TestLegacyRuntimeCannotUseUnauthenticatedShutdownWithProcessIdentity(t *testing.T) {
	addr, mux := startMockDaemon(t)
	var requests atomic.Int32
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: daemonServiceName, PID: os.Getpid()})
	})
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	info := &RuntimeInfo{
		PID:             os.Getpid(),
		Network:         "tcp",
		Address:         addr,
		processIdentity: currentProcessIdentity(t),
		legacyRecord:    true,
	}
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })

	err := requestUnauthenticatedDaemonShutdown(t.Context(), info, info.Endpoint(), func() bool { return false })
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	assert.Zero(t, requests.Load(), "legacy records must not contact an endpoint during unauthenticated shutdown")
}

func TestProcessIdentityHelper(t *testing.T) {
	if os.Getenv("ROBOREV_PROCESS_IDENTITY_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestUnauthenticatedShutdownRequiresPingPIDToMatchRuntime(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	addr, mux := startMockDaemon(t)
	var shutdownCalls atomic.Int32
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: daemonServiceName, PID: os.Getpid() + 1})
	})
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		shutdownCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	info := &RuntimeInfo{
		PID:             os.Getpid(),
		Network:         "tcp",
		Address:         addr,
		Service:         daemonServiceName,
		processIdentity: currentProcessIdentity(t),
	}
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })

	err := requestUnauthenticatedDaemonShutdown(t.Context(), info, info.Endpoint(), func() bool { return false })
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	assert.Zero(t, shutdownCalls.Load())
}

func TestUnauthenticatedShutdownRequiresRuntimeProcessIdentity(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	addr, mux := startMockDaemon(t)
	var requests atomic.Int32
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: daemonServiceName, PID: os.Getpid()})
	})
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	info := &RuntimeInfo{
		PID:     os.Getpid(),
		Network: "tcp",
		Address: addr,
		Service: daemonServiceName,
	}
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })

	err := requestUnauthenticatedDaemonShutdown(t.Context(), info, info.Endpoint(), func() bool { return false })
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	assert.Zero(t, requests.Load())
}

func currentProcessIdentity(t *testing.T) kitdaemon.ProcessIdentity {
	t.Helper()
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	return identity
}
