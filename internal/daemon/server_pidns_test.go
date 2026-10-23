//go:build linux

package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestKillDaemonKeepsForeignRuntimeWithoutShutdown(t *testing.T) {
	if address := os.Getenv("ROBOREV_TEST_PIDNS_STOP_ADDRESS"); address != "" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		require.Equal(t, 2, os.Getpid())
		require.NoError(t, WriteRuntime(DaemonEndpoint{Network: "tcp", Address: address}, nil, "test", nil))
		require.NoError(t, os.WriteFile(filepath.Join(config.DataDir(), "ready"), nil, 0o600))
		_, err := io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)
		return
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	var shutdownRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			_, _ = io.WriteString(w, `{"ok":true,"service":"roborev","pid":2}`)
		case "/api/shutdown":
			shutdownRequests.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("ROBOREV_TEST_PIDNS_STOP_ADDRESS", strings.TrimPrefix(server.URL, "http://"))
	setShortRuntimeDir(t)
	testutil.RunPIDNamespaceTest(t, nil, filepath.Join(config.DataDir(), "ready"))

	records, err := runtimeStore().List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(records[0]))
	info := runtimeInfoFromRecord(records[0])
	before, err := os.ReadFile(info.SourcePath)
	require.NoError(t, err)
	ping, err := ProbeDaemon(info.Endpoint(), time.Second)
	require.NoError(t, err)
	require.Equal(t, 2, ping.PID)

	err = KillDaemon(info)
	assert := assert.New(t)
	require.EqualError(t, err, fmt.Sprintf("cannot judge daemon PID %d from this PID namespace; stop it from where it runs", info.PID))
	assert.Equal(int32(0), shutdownRequests.Load())
	after, err := os.ReadFile(info.SourcePath)
	require.NoError(t, err)
	assert.Equal(before, after)
}

func TestDiscoveryFindsLiveSandboxDaemon(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_PIDNS_SERVE") == "1" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		require.Equal(t, 2, os.Getpid())
		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ServerAddr = "127.0.0.1:0"
		server := NewServer(db, cfg, "")
		t.Cleanup(func() { _ = server.Close() })
		errCh, _ := startServerAndWaitForRuntime(t, server)
		require.NoError(t, os.WriteFile(filepath.Join(config.DataDir(), "ready"), nil, 0o600))
		_, err := io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)
		stopTestServer(t, server, errCh)
		return
	}

	t.Setenv("HOME", t.TempDir())
	dataDir := testenv.SetDataDir(t)
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", dataDir)
	t.Setenv("ROBOREV_TEST_PIDNS_SERVE", "1")
	setShortRuntimeDir(t)
	testutil.RunPIDNamespaceTest(t, nil, filepath.Join(config.DataDir(), "ready"))

	records, err := runtimeStore().List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(records[0]))
	path := records[0].SourcePath
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := GetAnyRunningDaemon()
	require.NoError(t, err)
	assert.Equal(t, 2, info.PID)
	response, err := info.Endpoint().APIClient(time.Second).GetStatusRaw(t.Context())
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusOK, response.StatusCode)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestCleanupZombieDaemonsRemovesDeadSandboxSocket(t *testing.T) {
	if socket := os.Getenv("ROBOREV_TEST_PIDNS_SOCKET"); socket != "" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		require.Equal(t, 3, os.Getpid())
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: socket})
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		require.NoError(t, WriteRuntime(DaemonEndpoint{Network: "unix", Address: socket}, nil, "test", nil))
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGKILL))
		return
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	socket := filepath.Join(setShortRuntimeDir(t), "sandbox.sock")
	t.Setenv("ROBOREV_TEST_PIDNS_SOCKET", socket)
	// The shell exits successfully only after the sandbox daemon is killed.
	setShortRuntimeDir(t)
	testutil.RunPIDNamespaceTest(t, []string{"sh", "-c", `"$@"; test "$?" -eq 137`, "sh"}, "")
	records, err := runtimeStore().List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, kitdaemon.ProcessScopeGone, kitdaemon.RuntimeProcessScope(records[0]))
	require.True(t, kitdaemon.ProcessAlive(records[0].PID), "host must have an unrelated process at the sandbox PID")
	require.FileExists(t, socket)

	assert.Equal(t, 1, CleanupZombieDaemons(DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:0"}))
	assert.NoFileExists(t, records[0].SourcePath)
	assert.NoFileExists(t, socket)
}

func TestServerStartAfterShellLaunchedSandbox(t *testing.T) {
	if phase := os.Getenv("ROBOREV_TEST_PIDNS_PHASE"); phase != "" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		setShortRuntimeDir(t)
		if phase == "record" {
			require.Equal(t, 2, os.Getpid())
			require.NoError(t, WriteRuntime(DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, nil, "test", nil))
			require.NoError(t, os.WriteFile(filepath.Join(config.DataDir(), "ready"), nil, 0o600))
			_, err := io.Copy(io.Discard, os.Stdin)
			require.NoError(t, err)
			return
		}

		require.Equal(t, 2, os.Getppid(), "shell holds the old daemon's PID")
		require.Equal(t, 3, os.Getpid())
		records, err := runtimeStore().List()
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Equal(t, 2, records[0].PID)
		require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(records[0]))

		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ServerAddr = "127.0.0.1:0"
		server := NewServer(db, cfg, "")
		t.Cleanup(func() { _ = server.Close() })
		errCh, info := startServerAndWaitForRuntime(t, server)
		assert.Equal(t, 3, info.PID)
		assert.FileExists(t, filepath.Join(runtimeStore().Dir, "daemon.2.json"))
		stopTestServer(t, server, errCh)
		return
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	setShortRuntimeDir(t)
	t.Setenv("ROBOREV_TEST_PIDNS_PHASE", "record")
	// Keep the first namespace alive so its inode cannot be reused for start.
	testutil.RunPIDNamespaceTest(t, nil, filepath.Join(config.DataDir(), "ready"))
	t.Setenv("ROBOREV_TEST_PIDNS_PHASE", "start")
	// The trailing command keeps the shell at PID 2 while the daemon runs at PID 3.
	testutil.RunPIDNamespaceTest(t, []string{"sh", "-c", `"$@"; result=$?; exit "$result"`, "sh"}, "")
}
