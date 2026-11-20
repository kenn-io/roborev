//go:build unix

package scripts

import (
	"context"
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
	cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "true")
	cmd.Env = append(os.Environ(), "HOME=")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, string(out), "HOME must be set")
}

func TestPostgresRunRequiresLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		dryRun bool
	}{
		{"dry run", []string{"-n", "test-postgres"}, true},
		{"someone else's lease", []string{"test-postgres-run", "TEST_SERVICE_LEASE=other-1-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.dryRun {
				leaseDir := filepath.Join(dir, ".local/state/test-service-leases")
				require.NoError(t, os.MkdirAll(leaseDir, 0o700))
				lock, err := os.Create(filepath.Join(leaseDir, "other-1-1"))
				require.NoError(t, err)
				require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX))
				t.Cleanup(func() { _ = lock.Close() })
			}
			dockerLog := filepath.Join(dir, "docker-called")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ntouch \"$DOCKER_LOG\"\nexit 1\n"), 0o700))
			cmd := exec.CommandContext(t.Context(), "make", tc.args...)
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+dockerLog)
			out, err := cmd.CombinedOutput()
			if tc.dryRun {
				require.NoError(t, err, string(out))
				_, err = os.Stat(filepath.Join(dir, ".local/state/test-service-leases"))
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.Error(t, err, string(out))
				assert.Contains(t, string(out), "use make test-postgres")
			}
			_, err = os.Stat(dockerLog)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestPostgresRunCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		wantError bool
	}{
		{"success", "success", false},
		{"startup failure", "up-fails", true},
		{"test failure", "test-fails", true},
		{"interruption", "interrupt", true},
		{"cleanup failure", "down-fails", true},
		{"port failure", "port-fails", true},
		{"empty port", "port-empty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			docker := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
case " $* " in
  *' up '*|*' down '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 8
    python3 -c 'import fcntl, os, sys
with open(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"])) as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(0)
 sys.exit(1)' || exit 8
    case " $* " in
      *' up '*) [ "$TEST_PG_PUBLISH" = 127.0.0.1::5432 ] || exit 8
        case "$MODE" in up-fails) exit 1 ;; interrupt) kill -TERM "${TEST_SERVICE_LEASE##*-}" ;; esac ;;
      *' down '*) [ "$MODE" != down-fails ] || exit 1 ;;
    esac ;;
  *' port '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 9
    case "$MODE" in port-fails) echo 'port lookup failed' >&2; exit 1 ;; port-empty) exit 0 ;; esac
    echo 127.0.0.1:45678 ;;
esac
`
			goTool := `#!/bin/sh
if [ "$1" = env ]; then exit 0; fi
printf '%s\n' "$TEST_POSTGRES_URL" >> "$HOME/urls"
[ "$MODE" != test-fails ]
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goTool), 0o700))
			log := filepath.Join(dir, "docker.log")
			cmd := exec.CommandContext(t.Context(), "make", "test-postgres", "TEST_SERVICE_LEASE=other-1-1", "TEST_POSTGRES_URL=postgres://wrong")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log, "MODE="+tc.mode)
			out, err := cmd.CombinedOutput()
			if tc.wantError {
				require.Error(t, err, string(out))
			} else {
				require.NoError(t, err, string(out))
			}
			switch tc.mode {
			case "port-fails":
				assert.Contains(t, string(out), "port lookup failed")
			case "port-empty":
				assert.Contains(t, string(out), "could not determine postgres port")
			}
			urls, err := os.ReadFile(filepath.Join(dir, "urls"))
			switch tc.mode {
			case "port-fails", "port-empty":
				require.ErrorIs(t, err, os.ErrNotExist)
			case "success":
				require.NoError(t, err)
				for line := range strings.SplitSeq(strings.TrimSpace(string(urls)), "\n") {
					assert.Equal(t, "postgres://roborev_test:roborev_test_password@127.0.0.1:45678/roborev_test", line)
				}
			}
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			fields := strings.Fields(lines[0])
			require.GreaterOrEqual(t, len(fields), 3)
			lease := fields[2]
			assert.NotEqual(t, "other-1-1", lease)
			assert.Contains(t, lines[0], "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait postgres")
			entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
			require.NoError(t, err)
			if tc.mode == "down-fails" {
				require.Len(t, entries, 1)
				assert.Equal(t, lease, entries[0].Name())
			} else {
				assert.Empty(t, entries)
			}
			assert.Equal(t, "compose -p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml down", lines[len(lines)-1])
		})
	}
}

func TestPostgresCleanupWaitsForDescendant(t *testing.T) {
	for _, killed := range []bool{false, true} {
		name := "command fails"
		if killed {
			name = "supervisor killed"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			ready := filepath.Join(dir, "ready")
			release := filepath.Join(dir, "release")
			require.NoError(t, syscall.Mkfifo(release, 0o600))
			docker := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
case " $* " in *' port '*) echo 127.0.0.1:45678 ;; esac
`
			goTool := `#!/bin/sh
if [ "$1" = env ]; then exit 0; fi
python3 -c 'import os, signal
if os.fork() == 0:
 signal.alarm(20)
 devnull = os.open("/dev/null", os.O_WRONLY)
 os.dup2(devnull, 1)
 os.dup2(devnull, 2)
 with open(os.environ["READY"] + ".tmp", "w") as f: f.write(str(os.getpid()))
 os.rename(os.environ["READY"] + ".tmp", os.environ["READY"])
 with open(os.environ["RELEASE"]) as f: f.read()
 os._exit(0)' lease-cleanup-test-child
exit 7
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goTool), 0o700))
			log := filepath.Join(dir, "docker.log")
			cmd := exec.CommandContext(t.Context(), "make", "test-postgres")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log, "READY="+ready, "RELEASE="+release)
			require.NoError(t, cmd.Start())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			finished := false
			require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond)
			child, err := os.ReadFile(ready)
			require.NoError(t, err)
			pid, err := strconv.Atoi(string(child))
			require.NoError(t, err)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				args, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
				if err == nil && strings.Contains(string(args), "lease-cleanup-test-child") {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				if !finished {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						_ = cmd.Process.Kill()
						<-done
					}
				}
			})
			leaseDir := filepath.Join(dir, ".local/state/test-service-leases")
			entries, err := os.ReadDir(leaseDir)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			leasePath := filepath.Join(leaseDir, entries[0].Name())
			lease, err := os.Open(leasePath)
			require.NoError(t, err)
			defer lease.Close()
			if killed {
				parts := strings.Split(entries[0].Name(), "-")
				supervisor, err := strconv.Atoi(parts[len(parts)-1])
				require.NoError(t, err)
				args, err := exec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(supervisor), "-o", "args=").Output()
				require.NoError(t, err)
				require.Contains(t, string(args), "test-service-leases")
				require.NoError(t, syscall.Kill(supervisor, syscall.SIGKILL))
				select {
				case <-done:
					finished = true
				case <-time.After(10 * time.Second):
					t.Fatal("supervisor did not exit")
				}
			} else {
				time.Sleep(200 * time.Millisecond)
				select {
				case err := <-done:
					finished = true
					t.Fatalf("run exited before its child: %v", err)
				default:
				}
			}
			require.ErrorIs(t, syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			assert.NotContains(t, string(calls), " down")
			var fd int
			require.Eventually(t, func() bool {
				fd, err = syscall.Open(release, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
				return err == nil
			}, 10*time.Second, 10*time.Millisecond)
			require.NoError(t, os.NewFile(uintptr(fd), release).Close())
			if killed {
				require.Eventually(t, func() bool { return syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil }, 10*time.Second, 10*time.Millisecond)
				_, err = os.Stat(leasePath)
				require.NoError(t, err)
			} else {
				select {
				case err := <-done:
					finished = true
					var exit *exec.ExitError
					require.ErrorAs(t, err, &exit)
					assert.Equal(t, 2, exit.ExitCode(), "the failed make command must stay failed")
				case <-time.After(10 * time.Second):
					t.Fatal("cleanup did not finish after the child exited")
				}
				calls, err = os.ReadFile(log)
				require.NoError(t, err)
				assert.Contains(t, string(calls), " down\n")
			}
		})
	}
}
