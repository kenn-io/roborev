//go:build linux

package daemon

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestServerStartOverDeadSandboxRecord(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	testenv.SetDataDir(t)
	address := filepath.Join(setShortRuntimeDir(t), "daemon.sock")
	var rec kitdaemon.RuntimeRecord
	// End the sandbox before trying to start its replacement.
	t.Run("sandbox", func(t *testing.T) {
		rec = testutil.UnjudgedRuntimeRecord(t, "unix", address)
	})
	if rec.PID == 0 {
		t.Skip("requires PID namespaces")
	}
	_, err := runtimeStore().Write(rec)
	require.NoError(t, err)
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "unix://" + address
	cfg.AuthKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, config.SaveGlobal(cfg))
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { _ = server.Close() })
	errCh, info := startServerAndWaitForRuntime(t, server)
	assert.Equal(t, address, info.Address)
	stopTestServer(t, server, errCh)
}

func TestDiscoverySkipsLiveSandboxDaemon(t *testing.T) {
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
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, info)
	runtimes, err := ListAllRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)
	assert.True(t, runtimes[0].unjudged())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestServerStartAfterShellLaunchedSandbox(t *testing.T) {
	if phase := os.Getenv("ROBOREV_TEST_PIDNS_PHASE"); phase != "" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		setShortRuntimeDir(t)
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
	// Keep the first namespace alive so its inode cannot be reused for start.
	rec := testutil.UnjudgedRuntimeRecord(t, "tcp", "127.0.0.1:1")
	_, err := runtimeStore().Write(rec)
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_RUNTIME_WRITER", "")
	t.Setenv("ROBOREV_TEST_PIDNS_PHASE", "start")
	// The trailing command keeps the shell at PID 2 while the daemon runs at PID 3.
	testutil.RunPIDNamespaceTest(t, []string{"sh", "-c", `"$@"; result=$?; exit "$result"`, "sh"}, "")
}

func TestSandboxDiscoveryKeepsHostRuntime(t *testing.T) {
	if dataDir := os.Getenv("ROBOREV_TEST_HOST_RUNTIME_DATA"); dataDir != "" {
		t.Setenv("ROBOREV_DATA_DIR", dataDir)
		info, err := GetAnyRunningDaemon()
		require.ErrorIs(t, err, os.ErrNotExist)
		assert.Nil(t, info)
		assert.Zero(t, CleanupZombieDaemons(DaemonEndpoint{}))
		return
	}

	t.Setenv("HOME", t.TempDir())
	dataDir := testenv.SetDataDir(t)
	setShortRuntimeDir(t)
	require.NoError(t, WriteRuntime(DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, nil, "test", nil))
	path := RuntimePath()
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_HOST_RUNTIME_DATA", dataDir)
	testutil.RunPIDNamespaceTest(t, nil, "")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
