package agenthook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/version"
)

func TestLiveDaemonRecordsExcludesWrongServiceSelfAndDead(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	self := os.Getpid()

	writeRuntimeRecord(t, kitdaemon.RuntimeRecord{
		PID: self, Network: "tcp", Address: "127.0.0.1:1", Service: "roborev",
	})
	writeRuntimeRecord(t, kitdaemon.RuntimeRecord{
		PID: self, Network: "tcp", Address: "127.0.0.1:2", Service: ServiceName,
	})
	writeRuntimeRecord(t, kitdaemon.RuntimeRecord{
		PID: deadPID(t), Network: "tcp", Address: "127.0.0.1:3", Service: ServiceName,
	})

	records, err := liveDaemonRecords()
	require.NoError(t, err)
	assert.Empty(t, records)
	assert.NoError(t, assertNoLiveDaemonRecords())
}

func TestLiveDaemonRecordsExcludesAliveButUnreachable(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	pid := startSleeper(t)

	// A stale runtime file whose PID was reused by an unrelated process: the
	// PID is alive, but nothing answers the agent hook ping at its address.
	writeRuntimeRecord(t, kitdaemon.RuntimeRecord{
		PID: pid, Network: "tcp", Address: "127.0.0.1:1", Service: ServiceName,
	})
	stale, err := runtimeStore().Path(pid)
	require.NoError(t, err)
	require.FileExists(t, stale)

	records, err := liveDaemonRecords()

	require.NoError(t, err)
	assert.Empty(t, records, "an unreachable endpoint must not count as a live daemon")
	require.NoError(t, assertNoLiveDaemonRecords())
	assert.NoFileExists(t, stale, "stale runtime file should be removed")
}

func TestLiveDaemonRecordsDetectsReachableDaemon(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	pid := startSleeper(t)
	addr := startPingServer(t, ServiceName, version.Version, pid)

	writeRuntimeRecord(t, kitdaemon.RuntimeRecord{
		PID: pid, Network: "tcp", Address: addr, Service: ServiceName,
	})

	records, err := liveDaemonRecords()

	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, pid, records[0].PID)

	err = assertNoLiveDaemonRecords()
	require.Error(t, err)
	assert.Contains(t, err.Error(), daemonRestartHint)
}

func TestWriteDaemonStatusReportsUnreachableRecords(t *testing.T) {
	var buf bytes.Buffer
	records := []kitdaemon.RuntimeRecord{
		{PID: 4242, Network: "tcp", Address: "127.0.0.1:1", Service: ServiceName, Version: "test"},
	}

	require.NoError(t, writeDaemonStatus(&buf, records))

	var out daemonStatusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.True(t, out.Running)
	assert.Equal(t, 1, out.Count)
	assert.Equal(t, []int{4242}, out.PIDs)
	require.Len(t, out.Records, 1)
	assert.Equal(t, 4242, out.Records[0].PID)
	assert.Equal(t, "test", out.Records[0].Version)
	assert.False(t, out.Records[0].Reachable)
}

func TestWriteDaemonStatusReportsNotRunning(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeDaemonStatus(&buf, nil))

	var out daemonStatusOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.False(t, out.Running)
	assert.Equal(t, 0, out.Count)
	assert.Empty(t, out.Records)
}

func TestWaitForDaemonsExitReturnsWhenProcessesGone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	assert.NoError(t, waitForDaemonsExit(ctx, []int{deadPID(t)}))
	assert.NoError(t, waitForDaemonsExit(ctx, nil))
}

func TestWaitForDaemonsExitWaitsForLiveProcess(t *testing.T) {
	pid := startSleeper(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForDaemonsExit(ctx, []int{pid})

	require.Error(t, err, "must not report shutdown while the process is still alive")
	assert.ErrorIs(t, err, context.Canceled)
}

func writeRuntimeRecord(t *testing.T, rec kitdaemon.RuntimeRecord) {
	t.Helper()
	_, err := runtimeStore().Write(rec)
	require.NoError(t, err)
}

// startSleeper launches a long-lived process and returns its PID, giving tests
// a reliably alive PID that is not an agent hook daemon.
func startSleeper(t *testing.T) int {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	sleeper := exec.Command("sleep", "60")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_, _ = sleeper.Process.Wait()
	})
	return sleeper.Process.Pid
}

// deadPID returns the PID of a process that has already exited, which is a
// reliably non-live PID for filtering tests.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	require.NoError(t, cmd.Run())
	return cmd.Process.Pid
}
