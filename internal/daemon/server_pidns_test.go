//go:build linux

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	runPIDNamespaceTest(t, nil, true)

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
	runPIDNamespaceTest(t, nil, true)

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
		scope := kitdaemon.RuntimeProcessScope(records[0])
		if scope != kitdaemon.ProcessScopeForeign {
			// The kernel can reuse the record phase's namespace inode for this sandbox.
			assert.False(t, kitdaemon.RuntimeRecordBlocksStart(records[0]))
		}

		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ServerAddr = "127.0.0.1:0"
		server := NewServer(db, cfg, "")
		t.Cleanup(func() { _ = server.Close() })
		errCh, info := startServerAndWaitForRuntime(t, server)
		assert.Equal(t, 3, info.PID)
		if scope == kitdaemon.ProcessScopeForeign {
			assert.FileExists(t, filepath.Join(runtimeStore().Dir, "daemon.2.json"))
		}
		stopTestServer(t, server, errCh)
		return
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	for _, phase := range []string{"record", "start"} {
		t.Setenv("ROBOREV_TEST_PIDNS_PHASE", phase)
		var command []string
		if phase == "start" {
			// The trailing command keeps the shell at PID 2 while the daemon runs at PID 3.
			command = []string{"sh", "-c", `"$@"; result=$?; exit "$result"`, "sh"}
		}
		runPIDNamespaceTest(t, command, false)
	}
}

// runPIDNamespaceTest runs this test in a sandbox, optionally keeping it alive
// until cleanup after its runtime record or daemon listener is ready.
func runPIDNamespaceTest(t *testing.T, command []string, waitReady bool) {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("requires bubblewrap for PID namespaces")
	}
	args := []string{"--unshare-pid", "--bind", "/", "/", "--proc", "/proc", "--"}
	probe := exec.CommandContext(t.Context(), bwrap, append(args, "true")...)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("PID namespaces unavailable: %v: %s", err, output)
	}
	setShortRuntimeDir(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	command = append(command, executable, "-test.run=^"+t.Name()+"$", "-test.timeout=30s")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bwrap, append(args, command...)...)
	if !waitReady {
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		require.NoError(t, stdin.Close())
		require.NoError(t, cmd.Wait(), "%s", output.String())
	})
	// Wall-clock wait: the sandbox process writes its record or starts its listener.
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(config.DataDir(), "ready"))
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
}
