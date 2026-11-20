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
	path := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "true")
	cmd.Env = append(os.Environ(), "HOME=", "PATH="+path)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, string(out), "HOME must be set")
}

func TestPostgresRunRequiresLease(t *testing.T) {
	makePath := requireMake(t)
	for _, tc := range []struct {
		name        string
		args        []string
		dryRun      bool
		ignoreError bool
	}{
		{"dry run", []string{"-n", "test-postgres"}, true, false},
		{"question", []string{"-q", "test-postgres"}, true, true},
		{"touch", []string{"-t", "test-postgres"}, true, false},
		{"question inner", []string{"-q", "test-postgres-run"}, true, true},
		{"touch inner", []string{"-t", "test-postgres-run"}, true, false},
		{"ignored validation failure", []string{"-i", "test-postgres-run", "TEST_SERVICE_LEASE=other-1-1", "TEST_SERVICE_LEASE_FD=3"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.dryRun {
				requirePython(t)
			}
			dir := t.TempDir()
			var probe *os.File
			if !tc.dryRun {
				leaseDir := filepath.Join(dir, ".local/state/test-service-leases")
				require.NoError(t, os.MkdirAll(leaseDir, 0o700))
				leasePath := filepath.Join(leaseDir, "other-1-1")
				lock, err := os.Create(leasePath)
				require.NoError(t, err)
				require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX))
				t.Cleanup(func() { _ = lock.Close() })
				probe, err = os.OpenFile(leasePath, os.O_RDWR, 0)
				require.NoError(t, err)
				t.Cleanup(func() { _ = probe.Close() })
			}
			dockerLog := filepath.Join(dir, "docker-called")
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ntouch \"$DOCKER_LOG\"\nexit 1\n"), 0o700))
			cmd := exec.CommandContext(t.Context(), makePath, tc.args...)
			cmd.Dir = ".."
			if probe != nil {
				cmd.ExtraFiles = []*os.File{probe}
			}
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+dockerLog)
			out, err := cmd.CombinedOutput()
			if tc.dryRun && tc.ignoreError {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				assert.Equal(t, 1, exit.ExitCode())
			} else if tc.dryRun {
				require.NoError(t, err, string(out))
			}
			if tc.dryRun {
				_, err = os.Stat(filepath.Join(dir, ".local/state/test-service-leases"))
				require.ErrorIs(t, err, os.ErrNotExist)
			} else if tc.ignoreError {
				require.NoError(t, err, string(out))
			} else {
				require.Error(t, err, string(out))
			}
			if !tc.dryRun {
				assert.Contains(t, string(out), "use make test-postgres")
			}
			_, err = os.Stat(dockerLog)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestPostgresRunCleansUp(t *testing.T) {
	makePath := requireMake(t)
	requirePython(t)
	for _, tc := range []struct {
		name      string
		mode      string
		wantError bool
	}{
		{"success", "success", false},
		{"startup failure", "up-fails", true},
		{"test failure", "test-fails", true},
		{"port failure", "port-fails", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			docker := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
[ "$TEST_PG_PUBLISH" = 127.0.0.1::5432 ] || exit 8
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
        case "$MODE" in up-fails) exit 1 ;; esac ;;
      *' down '*) : ;;
    esac ;;
  *' port '*)
    [ "$2" = -p ] && [ "$3" = "$TEST_SERVICE_LEASE" ] || exit 9
    case " $* " in *' port postgres 5432 '*) ;; *) exit 9 ;; esac
    case "$MODE" in port-fails) echo 'port lookup failed' >&2; exit 1 ;; esac
    echo 127.0.0.1:45678 ;;
esac
`
			goTool := `#!/bin/sh
if [ "$1" = env ]; then exit 0; fi
if [ "$1" = tool ] && [ "$2" = gotestsum ]; then printf '%s\n' "$*" >> "$HOME/gotestsum.log"; fi
printf '%s\n' "$TEST_POSTGRES_URL" >> "$HOME/urls"
[ "$MODE" != test-fails ]
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goTool), 0o700))
			log := filepath.Join(dir, "docker.log")
			cmd := exec.CommandContext(t.Context(), makePath, "test-postgres", "TEST_SERVICE_LEASE=other-1-1", "TEST_POSTGRES_URL=postgres://wrong")
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log, "MODE="+tc.mode, "TEST_PG_PUBLISH=invalid")
			out, err := cmd.CombinedOutput()
			if tc.wantError {
				require.Error(t, err, string(out))
			} else {
				require.NoError(t, err, string(out))
			}
			urls, err := os.ReadFile(filepath.Join(dir, "urls"))
			switch tc.mode {
			case "port-fails", "up-fails":
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
			assert.Empty(t, entries)
			if tc.mode == "up-fails" {
				assert.NotContains(t, string(calls), " port ")
			}
			if tc.mode == "success" || tc.mode == "test-fails" {
				reportDir := filepath.Join("..", "gotestsum", lease)
				t.Cleanup(func() { _ = os.RemoveAll(reportDir) })
				reportCalls, err := os.ReadFile(filepath.Join(dir, "gotestsum.log"))
				require.NoError(t, err)
				if tc.mode == "success" {
					assert.Contains(t, string(reportCalls), "--jsonfile "+filepath.Join("gotestsum", lease, "postgres-storage.json"))
					assert.Contains(t, string(reportCalls), "--rerun-fails-report "+filepath.Join("gotestsum", lease, "postgres-storage-reruns.txt"))
					assert.Contains(t, string(reportCalls), "--jsonfile "+filepath.Join("gotestsum", lease, "postgres-daemon.json"))
					assert.Contains(t, string(reportCalls), "--rerun-fails-report "+filepath.Join("gotestsum", lease, "postgres-daemon-reruns.txt"))
				} else {
					assert.NotContains(t, string(reportCalls), "postgres-daemon")
				}
			}
			assert.Equal(t, "compose -p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml down", lines[len(lines)-1])
		})
	}
}

func TestLeaseCleanupLaunchErrorPreservesStatus(t *testing.T) {
	requirePython(t)
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	for _, tc := range []struct {
		command string
		status  int
	}{{"exit 0", 1}, {"exit 7", 7}} {
		t.Run(tc.command, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Symlink(python, filepath.Join(dir, "python3")))
			cmd := exec.CommandContext(t.Context(), "./scripts/test-lease.sh", "roborev", "/bin/sh", "-c", tc.command)
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, string(out))
			assert.Equal(t, tc.status, exit.ExitCode())
			assert.Contains(t, string(out), "test service cleanup failed")
			entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		})
	}
}

func TestPostgresMakeTerminationCleansUp(t *testing.T) {
	makePath := requireMake(t)
	requirePython(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	down := filepath.Join(dir, "down")
	docker := "#!/bin/sh\ncase \" $* \" in *' port '*) echo 127.0.0.1:45678 ;; *' down '*) touch \"$HOME/down\" ;; esac\n"
	goTool := "#!/bin/sh\nif [ \"$1\" = env ]; then exit 0; fi\ntouch \"$HOME/ready\"\nexec sleep 30\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goTool), 0o700))
	cmd := exec.CommandContext(t.Context(), makePath, "test-postgres")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 10*time.Second, 10*time.Millisecond)
	entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	reportDir := filepath.Join("..", "gotestsum", entries[0].Name())
	t.Cleanup(func() { _ = os.RemoveAll(reportDir) })
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-done:
		finished = true
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("make did not finish after termination")
	}
	_, err = os.Stat(down)
	require.NoError(t, err)
	entries, err = os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestLeaseCleanupAfterCommand(t *testing.T) {
	requirePython(t)
	for _, tc := range []struct {
		name, command, cleanup string
		status                 int
		retained               bool
	}{
		{"cleanup failure", "exit 0", "exit 1", 1, true},
		{"interruption", "kill -TERM \"$PPID\"; sleep 10", "exit 0", 143, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "docker.log")
			docker := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_LOG\"\n" + tc.cleanup + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
			cmd := exec.CommandContext(t.Context(), "./scripts/test-lease.sh", "roborev", "sh", "-c", tc.command)
			cmd.Dir = ".."
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, string(out))
			assert.Equal(t, tc.status, exit.ExitCode())
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			assert.Contains(t, string(calls), " down\n")
			entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
			require.NoError(t, err)
			if tc.retained {
				assert.Len(t, entries, 1)
			} else {
				assert.Empty(t, entries)
			}
		})
	}
}

func TestLeaseCleanupWaitsForDescendant(t *testing.T) {
	requirePython(t)
	for _, mode := range []string{"command fails", "supervisor killed", "signal while child holds lock"} {
		killed := mode == "supervisor killed"
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ready := filepath.Join(dir, "ready")
			release := filepath.Join(dir, "release")
			require.NoError(t, syscall.Mkfifo(release, 0o600))
			docker := `#!/bin/sh
printf '%s\n' "$*" >> "$DOCKER_LOG"
`
			childCommand := `#!/bin/sh
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
			require.NoError(t, os.WriteFile(filepath.Join(dir, "child"), []byte(childCommand), 0o700))
			log := filepath.Join(dir, "docker.log")
			require.NoError(t, os.WriteFile(log, nil, 0o600))
			cmd := exec.CommandContext(t.Context(), "./scripts/test-lease.sh", "roborev", filepath.Join(dir, "child"))
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
				if mode == "signal while child holds lock" {
					parts := strings.Split(entries[0].Name(), "-")
					supervisor, err := strconv.Atoi(parts[len(parts)-1])
					require.NoError(t, err)
					args, err := exec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(supervisor), "-o", "args=").Output()
					require.NoError(t, err)
					require.Contains(t, string(args), "test-service-leases")
					require.NoError(t, syscall.Kill(supervisor, syscall.SIGTERM))
					select {
					case err := <-done:
						finished = true
						var exit *exec.ExitError
						require.ErrorAs(t, err, &exit)
						assert.Equal(t, 143, exit.ExitCode())
					case <-time.After(10 * time.Second):
						t.Fatal("cleanup did not finish after the launcher was signalled")
					}
					calls, err := os.ReadFile(log)
					require.NoError(t, err)
					assert.Contains(t, string(calls), " down\n")
					_, err = os.Stat(leasePath)
					require.ErrorIs(t, err, os.ErrNotExist)
					return
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
					assert.Equal(t, 7, exit.ExitCode())
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

func TestLeaseCleanupFinishesAfterTerminalSignal(t *testing.T) {
	requirePython(t)
	dir := t.TempDir()
	started := filepath.Join(dir, "cleanup-started")
	release := filepath.Join(dir, "cleanup-release")
	finished := filepath.Join(dir, "cleanup-finished")
	docker := `#!/bin/sh
case " $* " in
  *' down '*)
    python3 -c 'import fcntl, os, sys
with open(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"]), "r+") as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(0)
 sys.exit(1)' || exit 9
    touch "$HOME/cleanup-started"
    while [ ! -e "$HOME/cleanup-release" ]; do sleep 0.01; done
    touch "$HOME/cleanup-finished" ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700))
	cmd := exec.CommandContext(t.Context(), "./scripts/test-lease.sh", "roborev", "true")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	finishedProcess := false
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		if !finishedProcess {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 10*time.Second, 10*time.Millisecond)
	leaseDir := filepath.Join(dir, ".local/state/test-service-leases")
	entries, err := os.ReadDir(leaseDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	lease := entries[0].Name()
	leasePath := filepath.Join(leaseDir, lease)
	lock, err := os.OpenFile(leasePath, os.O_RDWR, 0)
	require.NoError(t, err)
	require.ErrorIs(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
	require.NoError(t, lock.Close())
	args, err := exec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(cmd.Process.Pid), "-o", "args=").Output()
	require.NoError(t, err)
	require.Contains(t, strings.Fields(string(args)), "-c")
	require.NoError(t, syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM))
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		finishedProcess = true
		t.Fatalf("cleanup exited before it was released: %v", err)
	default:
	}
	_, err = os.Stat(finished)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	select {
	case err := <-done:
		finishedProcess = true
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		assert.Equal(t, 143, exit.ExitCode())
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not finish after release")
	}
	_, err = os.Stat(finished)
	require.NoError(t, err)
	_, err = os.Stat(leasePath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func requireMake(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"make", "gmake"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.CommandContext(t.Context(), path, "--version").Output()
		if err == nil && strings.Contains(string(out), "GNU Make") {
			return path
		}
	}
	t.Skip("GNU Make is not on PATH")
	return ""
}
