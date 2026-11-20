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

func TestLeaseSupervisorCleanup(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    int
	}{{"exit 0", 1}, {"exit 17", 17}} {
		t.Run(tc.command, func(t *testing.T) {
			dir := t.TempDir()
			cleanup := "exit 1"
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
			ready, cleaned, stopped := filepath.Join(dir, "ready"), filepath.Join(dir, "cleaned"), filepath.Join(dir, "stopped")
			cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", `touch "$CLEANED"`, "python3", "-c", `import os, signal, time
received = 0
def stop(sig, frame):
 global received
 received = sig
for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM): signal.signal(sig, stop)
child = os.fork()
if child == 0:
 def stop(sig, frame):
  with open(os.environ["STOPPED"], "w") as f: f.write(str(sig))
  os._exit(0)
 for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM): signal.signal(sig, stop)
 with open(os.environ["READY"], "w") as f: f.write(f"{os.getppid()} {os.getpid()}")
 while True: time.sleep(1)
os.waitpid(child, 0)
while not received: time.sleep(0.01)
exit(128 + received)`)
			cmd.Env = append(os.Environ(), "HOME="+dir, "READY="+ready, "CLEANED="+cleaned, "STOPPED="+stopped)
			require.NoError(t, cmd.Start())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var childPID, grandchildPID int
			waited := false
			t.Cleanup(func() {
				if waited {
					return
				}
				for _, pid := range []int{childPID, grandchildPID} {
					if pid == 0 {
						continue
					}
					ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
					args, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
					cancel()
					if err == nil && strings.Contains(string(args), "STOPPED") {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
				_ = cmd.Process.Kill()
				<-done
			})
			// Wait for the grandchild to install its signal handlers.
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(ready)
				if err != nil {
					return false
				}
				n, _ := fmt.Sscanf(string(data), "%d %d", &childPID, &grandchildPID)
				return n == 2
			}, 10*time.Second, 50*time.Millisecond)
			require.NoError(t, cmd.Process.Signal(tc.sig))
			var err error
			select {
			case err = <-done:
				waited = true
			case <-time.After(10 * time.Second):
				t.Fatal("supervisor did not stop after signal")
			}
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit)
			assert.Equal(t, tc.code, exit.ExitCode())
			data, err := os.ReadFile(stopped)
			require.NoError(t, err)
			assert.Equal(t, strconv.Itoa(int(tc.sig)), string(data))
			_, err = os.Stat(cleaned)
			require.NoError(t, err)
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
