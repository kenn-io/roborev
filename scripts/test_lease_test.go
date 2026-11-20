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

func TestPostgresLeaseOwnsCompose(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	docker := `#!/bin/sh
printf '%s [%s %s %s]\n' "$*" "$TEST_SERVICE_LEASE" "$COMPOSE_PROJECT_NAME" "$TEST_PG_PUBLISH" >> "$DOCKER_LOG"
case " $* " in
  *' up '*|*' down '*) python3 -c 'import fcntl, os, sys
with open(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"])) as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(0)
 sys.exit(1)' || exit 8 ;;
  *' port '*)
    case " $* " in *" -p $TEST_SERVICE_LEASE "*) echo 127.0.0.1:45678 ;; *) exit 9 ;; esac ;;
esac
`
	goTool := `#!/bin/sh
if [ "$1" = env ]; then exit 0; fi
printf '%s\n' "$TEST_POSTGRES_URL" >> "$URL_LOG"
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0o700))
	urlLog := filepath.Join(dir, "urls")
	dockerLog := filepath.Join(dir, "docker.log")
	cmd := exec.CommandContext(t.Context(), "make", "test-postgres", "TEST_SERVICE_LEASE=other-1-1", "TEST_POSTGRES_URL=postgres://wrong")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "URL_LOG="+urlLog, "DOCKER_LOG="+dockerLog)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	urls, err := os.ReadFile(urlLog)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(urls)), "\n")
	require.NotEmpty(t, strings.TrimSpace(string(urls)))
	for _, line := range lines {
		assert.Equal(t, "postgres://roborev_test:roborev_test_password@127.0.0.1:45678/roborev_test", line)
	}
	data, err := os.ReadFile(dockerLog)
	require.NoError(t, err)
	fields := strings.Fields(string(data))
	require.GreaterOrEqual(t, len(fields), 3)
	lease := fields[2]
	assert.NotEqual(t, "other-1-1", lease)
	assert.Contains(t, string(data), "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait postgres ["+lease+" "+lease+" 127.0.0.1::5432]")
	labels, err := os.ReadFile("../docker-compose.test-lease.yml")
	require.NoError(t, err)
	assert.Contains(t, string(labels), `test-service.lease: "${TEST_SERVICE_LEASE}"`)
}

func TestPostgresRunRequiresLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		dryRun bool
	}{
		{"dry run", []string{"-n", "test-postgres"}, true},
		{"empty lease", []string{"test-postgres-run", "TEST_SERVICE_LEASE="}, false},
		{"invented lease", []string{"test-postgres-run", "TEST_SERVICE_LEASE=other-1-1"}, false},
		{"someone else's lease", []string{"test-postgres-run", "TEST_SERVICE_LEASE=other-1-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.name == "someone else's lease" {
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

func TestPostgresPortLookupFailureStopsTests(t *testing.T) {
	for _, mode := range []string{"failure", "empty"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			docker := "#!/bin/sh\ncase \" $* \" in *' port '*) [ \"$MODE\" = empty ] && exit 0; echo 'port lookup failed' >&2; exit 1 ;; esac\n"
			goTool := "#!/bin/sh\nif [ \"$1\" = env ]; then exit 0; fi\ntouch \"$HOME/tests-ran\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goTool), 0o700))
			cmd := exec.CommandContext(t.Context(), "make", "test-postgres")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MODE="+mode)
			out, err := cmd.CombinedOutput()
			require.Error(t, err, string(out))
			if mode == "failure" {
				assert.Contains(t, string(out), "port lookup failed")
			}
			_, err = os.Stat(filepath.Join(dir, "tests-ran"))
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			docker := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
case " $* " in
  *' up '*)
    case "$MODE" in up-fails) exit 1 ;; interrupt) kill -TERM "${TEST_SERVICE_LEASE##*-}" ;; esac ;;
  *' port '*) echo 127.0.0.1:45678 ;;
  *' down '*) [ "$MODE" != down-fails ] || exit 1 ;;
esac
`
			goTool := "#!/bin/sh\nif [ \"$1\" = env ]; then exit 0; fi\n[ \"$MODE\" != test-fails ]\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goTool), 0o700))
			log := filepath.Join(dir, "docker.log")
			cmd := exec.CommandContext(t.Context(), "make", "test-postgres")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log, "MODE="+tc.mode)
			out, err := cmd.CombinedOutput()
			if tc.wantError {
				require.Error(t, err, string(out))
			} else {
				require.NoError(t, err, string(out))
			}
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
			fields := strings.Fields(lines[0])
			require.GreaterOrEqual(t, len(fields), 3)
			lease := fields[2]
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
	// The test command has failed, but its child still owns the lease.
	time.Sleep(200 * time.Millisecond)
	calls, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.NotContains(t, string(calls), " down")
	select {
	case err := <-done:
		finished = true
		t.Fatalf("run exited before its child: %v", err)
	default:
	}
	var fd int
	require.Eventually(t, func() bool {
		fd, err = syscall.Open(release, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, os.NewFile(uintptr(fd), release).Close())
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
