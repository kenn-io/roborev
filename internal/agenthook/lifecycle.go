package agenthook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
)

const daemonRestartHint = "roborev agent-hook daemon restart"

type restartOutput struct {
	Stopped []int `json:"stopped,omitempty"`
	PID     int   `json:"pid"`
}

type daemonStatusOutput struct {
	Running bool                 `json:"running"`
	Count   int                  `json:"count"`
	PIDs    []int                `json:"pids,omitempty"`
	Records []daemonStatusRecord `json:"records,omitempty"`
}

type daemonStatusRecord struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version,omitempty"`
	Network   string    `json:"network,omitempty"`
	Address   string    `json:"address,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Reachable bool      `json:"reachable"`
	PingPID   int       `json:"ping_pid,omitempty"`
}

// RunDaemonStart starts a detached agent hook daemon, refusing to start a
// duplicate when one is already running.
func RunDaemonStart(stdout io.Writer) error {
	records, err := liveDaemonRecords()
	if err != nil {
		return err
	}
	switch len(records) {
	case 0:
	case 1:
		return writeDaemonStatus(stdout, records)
	default:
		return fmt.Errorf("multiple %s daemons are running (%v); use `%s` to replace them", ServiceName, pids(records), daemonRestartHint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exe, err := restartExecutable()
	if err != nil {
		return err
	}
	if err := startDetachedDaemonExecutable(ctx, exe); err != nil {
		return err
	}
	rec, err := waitForDaemon(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	return writeDaemonStatus(stdout, []kitdaemon.RuntimeRecord{rec})
}

// RunDaemonStatus prints live agent hook daemon records as JSON.
func RunDaemonStatus(stdout io.Writer) error {
	records, err := liveDaemonRecords()
	if err != nil {
		return err
	}
	return writeDaemonStatus(stdout, records)
}

// RunDaemonStop terminates every live agent hook daemon and reports the PIDs
// it stopped.
func RunDaemonStop(stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped, err := stopLiveDaemons(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		Stopped []int `json:"stopped"`
	}{Stopped: stopped})
}

// RunDaemonRestart stops any live agent hook daemon and starts a fresh one from
// the caller's binary.
func RunDaemonRestart(stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stopped, err := stopLiveDaemons(ctx)
	if err != nil {
		return err
	}
	exe, err := restartExecutable()
	if err != nil {
		return err
	}
	if err := startDetachedDaemonExecutable(ctx, exe); err != nil {
		return err
	}
	rec, err := waitForDaemon(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(restartOutput{
		Stopped: stopped,
		PID:     rec.PID,
	})
}

func waitForDaemon(ctx context.Context, timeout time.Duration) (kitdaemon.RuntimeRecord, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		rec, _, ok, err := agentHookDaemonManager().Find(ctx)
		if err != nil {
			lastErr = err
		} else if ok {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return kitdaemon.RuntimeRecord{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("agent hook daemon failed to start within %s: %w", timeout, lastErr)
	}
	return kitdaemon.RuntimeRecord{}, fmt.Errorf("agent hook daemon failed to start within %s", timeout)
}

func writeDaemonStatus(stdout io.Writer, records []kitdaemon.RuntimeRecord) error {
	out := daemonStatusOutput{
		Running: len(records) > 0,
		Count:   len(records),
		PIDs:    pids(records),
		Records: make([]daemonStatusRecord, 0, len(records)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, rec := range records {
		status := daemonStatusRecord{
			PID:       rec.PID,
			Version:   rec.Version,
			Network:   rec.Network,
			Address:   rec.Address,
			StartedAt: rec.StartedAt,
		}
		info, err := kitdaemon.Probe(ctx, rec.Endpoint(), kitdaemon.ProbeOptions{
			ExpectedService: ServiceName,
			Timeout:         250 * time.Millisecond,
		})
		if err == nil {
			status.Reachable = true
			status.PingPID = info.PID
		}
		out.Records = append(out.Records, status)
	}
	return json.NewEncoder(stdout).Encode(out)
}

func pids(records []kitdaemon.RuntimeRecord) []int {
	ids := make([]int, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.PID)
	}
	return ids
}

func assertNoLiveDaemonRecords() error {
	records, err := liveDaemonRecords()
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s daemon already running as pid %d; use `%s` to replace it",
		ServiceName,
		records[0].PID,
		daemonRestartHint,
	)
}

// liveDaemonRecords returns runtime records that resolve to a reachable agent
// hook daemon. A record counts as live only when its endpoint answers the ping
// probe as the agent hook service reporting the PID it claims on disk. Probing
// the endpoint (rather than trusting that the recorded PID is merely alive)
// keeps stop/restart from signaling an unrelated process that reused a dead
// daemon's PID after a crash or reboot. Records that fail verification are
// removed as stale.
func liveDaemonRecords() ([]kitdaemon.RuntimeRecord, error) {
	records, err := runtimeStore().List()
	if err != nil {
		return nil, err
	}
	live := make([]kitdaemon.RuntimeRecord, 0, len(records))
	for _, rec := range records {
		if rec.Service != "" && rec.Service != ServiceName {
			continue
		}
		if rec.PID == os.Getpid() {
			continue
		}
		if !verifiedDaemonRecord(rec) {
			removeStaleRuntime(rec)
			continue
		}
		live = append(live, rec)
	}
	return live, nil
}

// verifiedDaemonRecord reports whether rec resolves to a live agent hook daemon:
// the recorded process must be alive and its endpoint must answer the ping probe
// as the agent hook service reporting the same PID recorded on disk.
func verifiedDaemonRecord(rec kitdaemon.RuntimeRecord) bool {
	if !kitdaemon.ProcessAlive(rec.PID) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := kitdaemon.Probe(ctx, rec.Endpoint(), kitdaemon.ProbeOptions{
		ExpectedService: ServiceName,
		Timeout:         750 * time.Millisecond,
	})
	return err == nil && info.PID == rec.PID
}

// removeStaleRuntime deletes the on-disk runtime file backing rec. It is
// best-effort: a record only reaches here after failing liveness verification,
// so a missing or unremovable file needs no further handling.
func removeStaleRuntime(rec kitdaemon.RuntimeRecord) {
	if rec.SourcePath == "" {
		return
	}
	_ = os.Remove(rec.SourcePath)
}

func stopLiveDaemons(ctx context.Context) ([]int, error) {
	records, err := liveDaemonRecords()
	if err != nil {
		return nil, err
	}
	stopped := make([]int, 0, len(records))
	for _, rec := range records {
		process, err := os.FindProcess(rec.PID)
		if err != nil {
			return nil, fmt.Errorf("find agent hook daemon pid %d: %w", rec.PID, err)
		}
		if err := process.Signal(syscall.SIGTERM); err != nil {
			return nil, fmt.Errorf("stop agent hook daemon pid %d: %w", rec.PID, err)
		}
		stopped = append(stopped, rec.PID)
	}
	if err := waitForDaemonsExit(ctx, stopped); err != nil {
		return nil, err
	}
	return stopped, nil
}

// waitForDaemonsExit blocks until every signaled pid has exited. Waiting on
// process exit rather than endpoint liveness ensures a stopped daemon finishes
// unwinding its shutdown defers - including removing its Unix socket - before a
// replacement binds the same path. Otherwise the old daemon's deferred socket
// removal could delete the replacement's socket, leaving the new daemon running
// but unreachable.
func waitForDaemonsExit(ctx context.Context, pids []int) error {
	for slices.ContainsFunc(pids, kitdaemon.ProcessAlive) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for agent hook daemon shutdown: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil
}
