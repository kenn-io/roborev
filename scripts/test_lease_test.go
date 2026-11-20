//go:build unix

package scripts

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
			cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "true")
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

func TestPostgresLeaseOwnsCompose(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	docker := `#!/bin/sh
printf '%s [%s %s %s]\n' "$*" "$TEST_SERVICE_LEASE" "$COMPOSE_PROJECT_NAME" "$TEST_PG_PUBLISH" >> "$DOCKER_LOG"
case " $* " in
  *' up '*) python3 -c 'import fcntl, os, sys
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
	cmd := exec.CommandContext(t.Context(), "make", "test-postgres", "TEST_SERVICE_LEASE=other-1-1")
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
	entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	lease := entries[0].Name()
	assert.NotEqual(t, "other-1-1", lease)
	data, err := os.ReadFile(dockerLog)
	require.NoError(t, err)
	assert.Contains(t, string(data), "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait postgres ["+lease+" "+lease+" 127.0.0.1::5432]")
	if runtime.GOOS == "darwin" && exec.CommandContext(t.Context(), "docker", "compose", "version").Run() != nil {
		t.Log("Docker Compose unavailable; skipping resolved label check")
		return
	}
	config := exec.CommandContext(t.Context(), "docker", "compose", "-p", lease, "-f", "docker-compose.test.yml", "-f", "docker-compose.test-lease.yml", "config", "--format", "json")
	config.Dir = ".."
	config.Env = append(os.Environ(), "TEST_SERVICE_LEASE="+lease, "COMPOSE_PROJECT_NAME="+lease, "TEST_PG_PUBLISH=127.0.0.1::5432")
	resolved, err := config.CombinedOutput()
	require.NoError(t, err, string(resolved))
	var compose struct {
		Services map[string]struct {
			Labels map[string]string `json:"labels"`
		} `json:"services"`
	}
	require.NoError(t, json.Unmarshal(resolved, &compose))
	assert.Equal(t, lease, compose.Services["postgres"].Labels["test-service.lease"])
}

func TestLeaseHeldByDescendantUntilExit(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	require.NoError(t, syscall.Mkfifo(release, 0o600))
	cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "python3", "-c", `import os, signal
if os.fork() == 0:
 signal.alarm(20)
 devnull = os.open("/dev/null", os.O_WRONLY)
 os.dup2(devnull, 1)
 os.dup2(devnull, 2)
 with open(os.environ["READY"] + ".tmp", "w") as f: f.write(str(os.getpid()))
 os.rename(os.environ["READY"] + ".tmp", os.environ["READY"])
 with open(os.environ["RELEASE"]) as f: f.read()
 os._exit(0)`, "lease-test-child")
	cmd.Env = append(os.Environ(), "HOME="+dir, "READY="+ready, "RELEASE="+release)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	// The child is an external process and publishes its readiness through a file.
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond)
	child, err := os.ReadFile(ready)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(child))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		args, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
		if err == nil && strings.Contains(string(args), "lease-test-child") {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	lease := filepath.Join(dir, ".local/state/test-service-leases", entries[0].Name())
	probe := func() error {
		f, err := os.Open(lease)
		if err != nil {
			return err
		}
		defer f.Close()
		return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	require.ErrorIs(t, probe(), syscall.EWOULDBLOCK, "descendant must hold the lease after the command exits")
	var fd int
	// The FIFO reader opens in the external child after it publishes readiness.
	require.Eventually(t, func() bool {
		fd, err = syscall.Open(release, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	f := os.NewFile(uintptr(fd), release)
	require.NoError(t, f.Close())
	// The external child exits after the FIFO closes and releases its inherited lock.
	require.Eventually(t, func() bool { return probe() == nil }, 10*time.Second, 10*time.Millisecond)
}

func TestLeaseRejectsPublicDirectory(t *testing.T) {
	dir := t.TempDir()
	leaseDir := filepath.Join(dir, ".local/state/test-service-leases")
	require.NoError(t, os.MkdirAll(leaseDir, 0o700))
	require.NoError(t, os.Chmod(leaseDir, 0o755))
	cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "touch", filepath.Join(dir, "started"))
	cmd.Env = append(os.Environ(), "HOME="+dir)
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "lease directory must be private")
	_, err = os.Stat(filepath.Join(dir, "started"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPostgresRunRequiresLease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		dryRun bool
	}{
		{"dry run", []string{"-n", "test-postgres"}, true},
		{"empty lease", []string{"test-postgres-run", "TEST_SERVICE_LEASE="}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dockerLog := filepath.Join(dir, "docker-called")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ntouch \"$DOCKER_LOG\"\nexit 1\n"), 0o700))
			cmd := exec.CommandContext(t.Context(), "make", tc.args...)
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+dockerLog)
			out, err := cmd.CombinedOutput()
			if tc.dryRun {
				require.NoError(t, err, string(out))
				_, err = os.Stat(filepath.Join(dir, ".local/state/test-service-leases"))
				assert.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.Error(t, err, string(out))
				assert.Contains(t, string(out), "use make test-postgres")
			}
			_, err = os.Stat(dockerLog)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
