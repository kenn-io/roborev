//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestServerStartAfterShellLaunchedSandbox(t *testing.T) {
	if phase := os.Getenv("ROBOREV_TEST_PIDNS_PHASE"); phase != "" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		setShortRuntimeDir(t)
		if phase == "record" {
			require.Equal(t, 2, os.Getpid())
			require.NoError(t, WriteRuntime(DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, nil, "test", nil))
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

	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("requires bubblewrap for PID namespaces")
	}
	args := []string{"--unshare-pid", "--bind", "/", "/", "--proc", "/proc", "--"}
	probe := exec.CommandContext(t.Context(), bwrap, append(args, "true")...)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("PID namespaces unavailable: %v: %s", err, output)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, phase := range []string{"record", "start"} {
		t.Setenv("ROBOREV_TEST_PIDNS_PHASE", phase)
		command := []string{executable, "-test.run=^TestServerStartAfterShellLaunchedSandbox$", "-test.timeout=30s"}
		if phase == "start" {
			// The trailing command keeps the shell at PID 2 while the daemon runs at PID 3.
			command = append([]string{"sh", "-c", `"$@"; result=$?; exit "$result"`, "sh"}, command...)
		}
		cmd := exec.CommandContext(t.Context(), bwrap, append(args, command...)...)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s sandbox: %s", phase, output)
	}
}
