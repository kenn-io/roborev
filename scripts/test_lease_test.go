//go:build unix

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresLeaseConcurrentAndInterrupted(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0700))
	log := filepath.Join(dir, "docker.log")
	docker := `#!/bin/sh
printf '%s [%s]\n' "$*" "$TEST_PG_PUBLISH" >> "$DOCKER_LOG"
case " $* " in
  *' up '*) touch "$READY_DIR/$3"; while [ ! -e "$READY_DIR/release" ]; do sleep .05; done ;;
  *' port '*) echo 127.0.0.1:45678 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 0\n"), 0700))
	start := func() *exec.Cmd {
		cmd := exec.Command("make", "test-postgres")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log, "READY_DIR="+dir, "GO_TEST_REPORT_DIR="+filepath.Join(dir, "reports"))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
			}
		})
		return cmd
	}
	first, second := start(), start()
	// Docker is a separate process and creates the readiness files.
	require.Eventually(t, func() bool {
		entries, _ := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
		if len(entries) != 2 {
			return false
		}
		_, err1 := os.Stat(filepath.Join(dir, entries[0].Name()))
		_, err2 := os.Stat(filepath.Join(dir, entries[1].Name()))
		return err1 == nil && err2 == nil
	}, 10*time.Second, 50*time.Millisecond)
	require.NoError(t, syscall.Kill(-first.Process.Pid, syscall.SIGTERM))
	_ = first.Wait()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release"), nil, 0600))
	require.NoError(t, second.Wait())
	entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.Contains(t, string(data), "-p "+entry.Name()+" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait postgres [127.0.0.1::5432]")
		assert.Contains(t, string(data), "-p "+entry.Name()+" -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans")
	}
}

func TestPostgresLeaseRecipe(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0700))
	log := filepath.Join(dir, "docker.log")
	docker := `#!/bin/sh
printf '%s [%s]\n' "$*" "$TEST_SERVICE_LEASE" >> "$DOCKER_LOG"
case " $* " in
  *' up '*) python3 -c 'import fcntl, os, sys
with open(os.path.join(os.environ["HOME"], ".local/state/test-service-leases", os.environ["TEST_SERVICE_LEASE"])) as f:
 try: fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
 except BlockingIOError: sys.exit(0)
 sys.exit(1)' || exit 8 ;;
  *' port '*) echo 127.0.0.1:45678 ;;
  *' down '*) exit 1 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 17\n"), 0700))
	cmd := exec.CommandContext(t.Context(), "make", "test-postgres", "TEST_SERVICE_LEASE=other-1-1")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+log, "GO_TEST_REPORT_DIR="+filepath.Join(dir, "reports"))
	out, err := cmd.CombinedOutput()
	require.Error(t, err, string(out))
	assert.Contains(t, string(out), "cleanup failed; scheduled cleanup will retry")
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	lease := entries[0].Name()
	assert.NotEqual(t, "other-1-1", lease)
	assert.Contains(t, string(data), "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml up -d --wait postgres ["+lease+"]")
	assert.Contains(t, string(data), "-p "+lease+" -f docker-compose.test.yml -f docker-compose.test-lease.yml down -v --remove-orphans")
	assert.Contains(t, string(out), "Error 17")
}

func TestPostgresLeaseCleanupAndReports(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	for _, tc := range []struct {
		name, goExit, downExit string
		wantError, wantReport  bool
	}{
		{"success", "0", "0", false, false},
		{"cleanup failure", "0", "1", true, true},
		{"test failure", "17", "0", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			require.NoError(t, os.Mkdir(bin, 0700))
			docker := `#!/bin/sh
case " $* " in
  *' port '*) echo 127.0.0.1:45678 ;;
  *' down '*) exit "$DOWN_EXIT" ;;
esac
`
			goTool := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--jsonfile" ]; then shift; printf report > "$1"; break; fi
  shift
done
exit "$GO_EXIT"
`
			require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0700))
			cmd := exec.CommandContext(t.Context(), "make", "test-postgres")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GO_TEST_REPORT_DIR="+filepath.Join(dir, "reports"), "GO_EXIT="+tc.goExit, "DOWN_EXIT="+tc.downExit)
			out, err := cmd.CombinedOutput()
			if tc.wantError {
				require.Error(t, err, string(out))
			} else {
				require.NoError(t, err, string(out))
			}
			entries, err := os.ReadDir(filepath.Join(dir, ".local/state/test-service-leases"))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			_, err = os.Stat(filepath.Join(dir, "reports", entries[0].Name(), "postgres-storage.json"))
			if tc.wantReport {
				assert.NoError(t, err)
			} else {
				assert.True(t, os.IsNotExist(err))
			}
		})
	}
}

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

func TestPostgresLeaseReportPaths(t *testing.T) {
	for _, lease := range []string{"roborev-1-1", "roborev-1-2"} {
		cmd := exec.CommandContext(t.Context(), "make", "-n", "test-postgres-ci", "GO_TEST_REPORT_DIR=reports", "TEST_SERVICE_LEASE="+lease)
		cmd.Dir = ".."
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		for _, name := range []string{"postgres-storage.json", "postgres-storage-reruns.txt", "postgres-daemon.json", "postgres-daemon-reruns.txt"} {
			assert.Contains(t, string(out), "reports/"+lease+"/"+name)
		}
	}
}

func TestPostgresLeaseOverridesCallerURLAndWritesSpacedReports(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0700))
	docker := `#!/bin/sh
case " $* " in
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
      printf report > "$1" || exit 1 ;;
  esac
  shift
done
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte(goTool), 0700))
	reports := filepath.Join(dir, "reports with spaces")
	urlLog := filepath.Join(dir, "urls")
	cmd := exec.CommandContext(t.Context(), "make", "test-postgres", "TEST_POSTGRES_URL=postgres://caller/example")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GO_TEST_REPORT_DIR="+reports, "URL_LOG="+urlLog, "REPORT_LOG="+filepath.Join(dir, "report-log"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	urls, err := os.ReadFile(urlLog)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("postgres://roborev_test:roborev_test_password@127.0.0.1:45678/roborev_test\n", 2), string(urls))
	reportNames, err := os.ReadFile(filepath.Join(dir, "report-log"))
	require.NoError(t, err)
	assert.Equal(t, "postgres-storage.json\npostgres-storage-reruns.txt\npostgres-daemon.json\npostgres-daemon-reruns.txt\n", string(reportNames))
}

func TestLeaseRequiresPython(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ntouch \"$DOCKER_LOG\"\n"), 0700))
	cmd := exec.CommandContext(t.Context(), "./test-lease.sh", "roborev", "docker")
	cmd.Env = append(os.Environ(), "PATH="+dir, "DOCKER_LOG="+log, "TEST_SERVICE_LEASE=")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.ExitCode())
	assert.Contains(t, string(out), "make postgres-up")
	_, err = os.Stat(log)
	assert.True(t, os.IsNotExist(err))
}

func TestPostgresManualUp(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "make", "-n", "postgres-up")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "docker compose -f docker-compose.test.yml up -d --wait")
}

func TestLeaseSurvivesGoTestLauncher(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.Mkdir(home, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/lease\n\ngo 1.24\n"), 0600))
	source := `package lease
import ("os"; "testing"; "time")
func TestWait(t *testing.T) {
    os.WriteFile(os.Getenv("READY"), nil, 0600)
    for { if _, err := os.Stat(os.Getenv("DONE")); err == nil { return }; time.Sleep(20*time.Millisecond) }
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lease_test.go"), []byte(source), 0600))
	cache, err := exec.CommandContext(t.Context(), "go", "env", "GOCACHE").Output()
	require.NoError(t, err)
	ready, done := filepath.Join(dir, "ready"), filepath.Join(dir, "done")
	t.Cleanup(func() { _ = os.WriteFile(done, nil, 0600) })
	cmd := exec.Command(filepath.Join(root, "scripts/test-lease.sh"), "roborev", "go", "test")
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
	require.NoError(t, os.WriteFile(done, nil, 0600))
	// The external test binary must exit before it releases the lock.
	require.Eventually(t, func() bool { return check() == nil }, 10*time.Second, 50*time.Millisecond)
}
