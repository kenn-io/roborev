//go:build unix

package scripts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaseRequiresHome(t *testing.T) {
	for _, home := range []string{"unset", "empty"} {
		t.Run(home, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "true", "true")
			for _, env := range os.Environ() {
				if !strings.HasPrefix(env, "HOME=") {
					cmd.Env = append(cmd.Env, env)
				}
			}
			if home == "empty" {
				cmd.Env = append(cmd.Env, "HOME=")
			}
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			assert.Equal(t, 2, exit.ExitCode())
			assert.Contains(t, string(out), "HOME must be set")
		})
	}
}

func TestPostgresLeaseOwnsComposeAndReportResources(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	docker := `#!/bin/sh
printf '%s [%s %s]\n' "$*" "$TEST_SERVICE_LEASE" "$TEST_PG_PUBLISH" >> "$DOCKER_LOG"
case " $* " in
  *' up '*) python3 -c 'import fcntl, os, sys
with open(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"])) as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(0)
 sys.exit(1)' || exit 8 ;;
  *' port '*) echo 127.0.0.1:45678 ;;
esac
`
	goTool := `#!/bin/sh
if [ "$1" = env ]; then exit 0; fi
printf '%s\n' "$TEST_POSTGRES_URL" >> "$URL_LOG"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --jsonfile|--rerun-fails-report)
      shift
      case "$1" in "$GO_TEST_REPORT_DIR/$TEST_SERVICE_LEASE/"*) ;; *) exit 2 ;; esac
      printf '%s\n' "${1##*/}" >> "$REPORT_LOG"
      mkdir -p "${1%/*}"
      printf report > "$1" || exit 1 ;;
  esac
  shift
done
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0o700))
	reports := filepath.Join(dir, "reports with spaces")
	urlLog := filepath.Join(dir, "urls")
	old := filepath.Join(reports, "roborev-1-1")
	recent := filepath.Join(reports, "roborev-2-2")
	for _, path := range []string{old, recent} {
		require.NoError(t, os.MkdirAll(path, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(path, "postgres-storage.json"), []byte("report"), 0o600))
	}
	past := time.Now().Add(-9 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(old, past, past))
	dockerLog := filepath.Join(dir, "docker.log")
	cmd := exec.CommandContext(t.Context(), "make", "test-postgres", "TEST_POSTGRES_URL=postgres://caller/example", "TEST_SERVICE_LEASE=other-1-1")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GO_TEST_REPORT_DIR="+reports, "URL_LOG="+urlLog, "REPORT_LOG="+filepath.Join(dir, "report-log"), "DOCKER_LOG="+dockerLog)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	urls, err := os.ReadFile(urlLog)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("postgres://roborev_test:roborev_test_password@127.0.0.1:45678/roborev_test\n", 2), string(urls))
	reportNames, err := os.ReadFile(filepath.Join(dir, "report-log"))
	require.NoError(t, err)
	assert.Equal(t, "postgres-storage.json\npostgres-storage-reruns.txt\npostgres-daemon.json\npostgres-daemon-reruns.txt\n", string(reportNames))
	entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	lease := entries[0].Name()
	assert.NotEqual(t, "other-1-1", lease)
	data, err := os.ReadFile(dockerLog)
	require.NoError(t, err)
	assert.Contains(t, string(data), "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait postgres ["+lease+" 127.0.0.1::5432]")
	assert.Contains(t, string(data), "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans")
	_, err = os.Stat(old)
	assert.True(t, os.IsNotExist(err), "old reports remained")
	_, err = os.Stat(filepath.Join(recent, "postgres-storage.json"))
	require.NoError(t, err)
}

func TestLeaseSurvivesGoTestLauncher(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/lease\n\ngo 1.24\n"), 0o600))
	source := `package lease
import ("os"; "testing"; "time")
func TestWait(t *testing.T) {
    os.WriteFile(os.Getenv("READY"), nil, 0600)
    for { if _, err := os.Stat(os.Getenv("DONE")); err == nil { return }; time.Sleep(20*time.Millisecond) }
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lease_test.go"), []byte(source), 0o600))
	cache, err := exec.CommandContext(t.Context(), "go", "env", "GOCACHE").Output()
	require.NoError(t, err)
	ready, done := filepath.Join(dir, "ready"), filepath.Join(dir, "done")
	t.Cleanup(func() { _ = os.WriteFile(done, nil, 0o600) })
	cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts/test-lease.sh"), "roborev", "true", "go", "test")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "GOCACHE="+strings.TrimSpace(string(cache)), "READY="+ready, "DONE="+done)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	// The child is an external test binary, so it cannot use virtual time.
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 60*time.Second, 50*time.Millisecond)
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	entries, err := os.ReadDir(filepath.Join(home, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	lease := filepath.Join(home, ".local/state/test-service-leases", entries[0].Name())
	probe := `import fcntl, sys
with open(sys.argv[1]) as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(1)
`
	check := func() error { return exec.CommandContext(t.Context(), "python3", "-c", probe, lease).Run() }
	require.Error(t, check())
	require.NoError(t, os.WriteFile(done, nil, 0o600))
	// The external test binary must exit before it releases the lock.
	require.Eventually(t, func() bool { return check() == nil }, 10*time.Second, 50*time.Millisecond)
}

func TestLeaseSupervisorCleanup(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{{"exit 0", 1}, {"exit 17", 17}} {
		t.Run(tc.command, func(t *testing.T) {
			dir := t.TempDir()
			cleanup := `python3 -c 'import fcntl, os, sys
with open(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"])) as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(0)
 sys.exit(99)' || exit 99
exit 1`
			cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", cleanup, "sh", "-c", tc.command)
			cmd.Env = append(os.Environ(), "HOME="+dir)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, string(out))
			assert.Equal(t, tc.want, exit.ExitCode(), string(out))
			assert.Contains(t, string(out), "cleanup failed; scheduled cleanup will retry")
		})
	}
	t.Run("descendant holds lease", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", `touch "$CLEANED"`, "sh", "-c", `sleep 60 >/dev/null 2>&1 & echo $!`)
		cmd.Env = append(os.Environ(), "HOME="+dir, "CLEANED="+filepath.Join(dir, "cleaned"))
		out, err := cmd.Output()
		if pid, convErr := strconv.Atoi(strings.TrimSpace(string(out))); convErr == nil {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
				defer cancel()
				args, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
				if err == nil && strings.TrimSpace(string(args)) == "sleep 60" {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
		}
		require.NoError(t, err, string(out))
		_, err = os.Stat(filepath.Join(dir, "cleaned"))
		assert.True(t, os.IsNotExist(err), "cleanup ran while a descendant held the lease")
	})
}

func TestLeaseSupervisorForwardsSignals(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGHUP, 129}, {syscall.SIGINT, 130}, {syscall.SIGTERM, 143}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			ready, cleaned := filepath.Join(dir, "ready"), filepath.Join(dir, "cleaned")
			cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", `touch "$CLEANED"`, "python3", "-c", `import os, signal, time
for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
 signal.signal(sig, lambda s, f: exit(128+s))
open(os.environ["READY"], "w").close()
while True: time.sleep(1)`)
			cmd.Env = append(os.Environ(), "HOME="+dir, "READY="+ready, "CLEANED="+cleaned)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			require.NoError(t, cmd.Start())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
				}
			})
			// Wait for the external child to install its signal handlers.
			require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 50*time.Millisecond)
			require.NoError(t, cmd.Process.Signal(tc.sig))
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				err = <-done
			}
			waited = true
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			assert.Equal(t, tc.code, exit.ExitCode())
			_, err = os.Stat(cleaned)
			require.NoError(t, err)
		})
	}
}

func TestPostgresCancellationStopsForegroundWork(t *testing.T) {
	for _, phase := range []string{"setup", "test"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			require.NoError(t, os.Mkdir(bin, 0o700))
			program := `#!/usr/bin/env python3
import os, signal, sys, time
name = os.path.basename(sys.argv[0])
if name == "docker" and "port" in sys.argv:
    print("127.0.0.1:45678")
    sys.exit(0)
if (name == "docker" and "up" in sys.argv and os.environ["PHASE"] == "setup") or (name == "go" and "tool" in sys.argv and os.environ["PHASE"] == "test"):
    def terminate(signum, frame):
        open(os.environ["STOPPED"], "w").close()
        sys.exit(0)
    signal.signal(signal.SIGTERM, terminate)
    ready_tmp = os.environ["READY"] + ".tmp"
    with open(ready_tmp, "w") as f:
        f.write(f'{os.environ["TEST_SERVICE_LEASE"].rsplit("-", 1)[1]} {os.getpid()}')
    os.replace(ready_tmp, os.environ["READY"])
    while True: time.sleep(1)
`
			for _, name := range []string{"docker", "go"} {
				require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(program), 0o700))
			}
			ready, stopped := filepath.Join(dir, "ready"), filepath.Join(dir, "stopped")
			cmd := exec.CommandContext(t.Context(), "make", "test-postgres")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "PHASE="+phase, "READY="+ready, "STOPPED="+stopped, "GO_TEST_REPORT_DIR="+filepath.Join(dir, "reports"))
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			require.NoError(t, cmd.Start())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var childPID int
			waited := false
			t.Cleanup(func() {
				if _, err := os.Stat(stopped); err != nil && childPID != 0 {
					_ = syscall.Kill(childPID, syscall.SIGKILL)
				}
				if !waited {
					_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					<-done
				}
			})
			// The fake child is an external process, so it cannot use virtual time.
			require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 50*time.Millisecond)
			data, err := os.ReadFile(ready)
			require.NoError(t, err)
			var wrapperPID int
			_, err = fmt.Sscanf(string(data), "%d %d", &wrapperPID, &childPID)
			require.NoError(t, err)
			require.NoError(t, syscall.Kill(wrapperPID, syscall.SIGTERM))
			select {
			case err := <-done:
				waited = true
				require.Error(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("make did not stop after cancellation")
			}
			require.Eventually(t, func() bool { _, err := os.Stat(stopped); return err == nil }, 3*time.Second, 50*time.Millisecond)
		})
	}
}

func TestPostgresRunRequiresLease(t *testing.T) {
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker-called")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ntouch \"$DOCKER_LOG\"\nexit 1\n"), 0o700))
	cmd := exec.CommandContext(t.Context(), "make", "test-postgres-run", "TEST_SERVICE_LEASE=")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+dockerLog, "GO_TEST_REPORT_DIR="+filepath.Join(dir, "reports"))
	out, err := cmd.CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "TEST_SERVICE_LEASE is required")
	_, err = os.Stat(dockerLog)
	assert.True(t, os.IsNotExist(err), "docker ran without a lease")
}
