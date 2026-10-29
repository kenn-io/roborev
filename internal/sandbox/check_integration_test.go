//go:build integration && linux

package sandbox

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
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

	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}

func TestSandboxSelfCheck(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	job := Job{WorkDir: t.TempDir()}
	err := job.check(t.Context(), []string{"env"}, []string{"PATH=/usr/bin:/bin"})
	require.ErrorContains(t, err, "sandbox self-check failed")
	for _, name := range []string{"canary-read", "loopback-connect", "external-connect", "environment-canary"} {
		assert.Contains(t, err.Error(), name)
	}
}

func TestSandboxLiteralGrants(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	for _, grant := range []string{"work", "read"} {
		t.Run(grant, func(t *testing.T) {
			base := t.TempDir()
			work, read := filepath.Join(base, "job"), filepath.Join(base, "read")
			if grant == "work" {
				work += "*"
			} else {
				read += "*"
			}
			sibling := filepath.Join(base, "job-private")
			secret := filepath.Join(base, "read-secret")
			for _, dir := range []string{work, read, sibling} {
				require.NoError(t, os.Mkdir(dir, 0o700))
			}
			require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(read, "allowed"), []byte("allowed"), 0o600))
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "home-secret"), []byte("secret"), 0o600))
			configDir := filepath.Join(work, "config")
			profiles := filepath.Join(configDir, "nono", "profiles")
			require.NoError(t, os.MkdirAll(profiles, 0o700))
			planted, err := json.Marshal(map[string]any{"filesystem": map[string]any{"read_file": []string{filepath.Join(home, "home-secret")}}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(profiles, "default.json"), planted, 0o600))
			repo, gitDir := filepath.Join(work, "repo"), filepath.Join(read, "repo.git")
			require.NoError(t, os.Mkdir(repo, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("sandbox fixture\n"), 0o600))
			for _, args := range [][]string{
				{"init", "--separate-git-dir=" + gitDir, repo},
				{"-C", repo, "add", "README.md"},
				{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "-m", "fixture"},
			} {
				out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
				require.NoError(t, err, "%s", out)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			other, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer other.Close()
			job := Job{WorkDir: work, ReadPaths: []string{read}, ProxyPort: listener.Addr().(*net.TCPAddr).Port,
				Env: []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_CONFIG_HOME=" + configDir}, MemoryMax: 1 << 30}
			script := `
failed=0
check() {
  name=$1 expected=$2; shift 2
  "$@" >/dev/null 2>&1; actual=$?
  if { [ "$expected" = allow ] && [ "$actual" -ne 0 ]; } || { [ "$expected" = deny ] && [ "$actual" -eq 0 ]; }; then
    echo "BAD $name"; failed=1
  else
    echo "OK $name"
  fi
}
check work-write allow touch "$1/allowed"
check read-grant allow cat "$2/allowed"
check sibling-write deny touch "$3/overwritten"
check sibling-read deny cat "$4"
check home-read deny cat "$HOME/home-secret"
check home-write deny touch "$HOME/write"
check shared-tmp-write deny bash -c 'p=/tmp/roborev-probe-$$; touch "$p" && rm "$p"'
cd "$5" || exit 1
check git-read allow git status --porcelain
check git-commit deny bash -c 'echo changed >> README.md && git add README.md && git -c user.name=Test -c user.email=test@example.com commit -m probe'
check git-config deny git config --local sandbox.probe changed
check dbus-systemd deny env XDG_RUNTIME_DIR="$6" DBUS_SESSION_BUS_ADDRESS="unix:path=$6/bus" systemd-run --user --wait -q /usr/bin/true
check external-domain deny curl -fsS https://example.com
check other-loopback deny bash -c ': >/dev/tcp/127.0.0.1/"$1"' probe "$7"
check proxy-port allow bash -c ': >/dev/tcp/127.0.0.1/"$1"' probe "$8"
exit "$failed"
`
			cmd, env, cleanup, err := job.Command(t.Context(), []string{"bash", "-c", script, "probe", work, read, sibling, secret, repo, os.Getenv("XDG_RUNTIME_DIR"), strconv.Itoa(other.Addr().(*net.TCPAddr).Port), strconv.Itoa(job.ProxyPort)})
			require.NoError(t, err)
			defer cleanup()
			process := exec.CommandContext(t.Context(), cmd[0], cmd[1:]...)
			process.Env, process.Dir = env, work
			out, err := process.CombinedOutput()
			require.NoError(t, err, "%s", out)
			assert.NotContains(t, string(out), "BAD ")
			for _, name := range []string{"work-write", "read-grant", "sibling-write", "sibling-read", "home-read", "home-write", "shared-tmp-write", "git-read", "git-commit", "git-config", "dbus-systemd", "external-domain", "other-loopback", "proxy-port"} {
				assert.Contains(t, string(out), "OK "+name)
			}
			assert.NoFileExists(t, filepath.Join(sibling, "overwritten"))
		})
	}
	t.Run("unenforceable-policy", func(t *testing.T) {
		job := Job{WorkDir: t.TempDir(), ProxyPort: 0, MemoryMax: 1 << 30,
			Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}}
		_, _, _, err := job.Command(t.Context(), []string{"true"})
		require.ErrorContains(t, err, "open_port")
	})
}

func TestSandboxProcessCleanup(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "return", true: "cancel"}[cancelRun], func(t *testing.T) {
			work := t.TempDir()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			job := Job{
				WorkDir: work, ProxyPort: listener.Addr().(*net.TCPAddr).Port,
				Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, MemoryMax: 1 << 30,
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			script := `setsid /usr/bin/sleep 300 </dev/null >/dev/null 2>&1 & echo $! > '` + filepath.Join(work, "child.pid") + `'`
			if cancelRun {
				script += "; wait"
			}
			wrapped, env, cleanup, err := job.Command(ctx, []string{"bash", "-c", script})
			require.NoError(t, err)
			defer cleanup()
			process := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
			process.Env, process.Dir = env, work
			require.NoError(t, process.Start())
			defer func() {
				cancel()
				_ = process.Wait()
			}()
			var pid int
			defer func() {
				// Clean up this test's child even when testing a broken wrapper.
				line, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
				if string(line) == "/usr/bin/sleep\x00300\x00" {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}()
			// The external shell must start the detached sleep before cancellation.
			require.Eventually(t, func() bool {
				data, err := os.ReadFile(filepath.Join(work, "child.pid"))
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil {
					return false
				}
				line, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
				return string(line) == "/usr/bin/sleep\x00300\x00"
			}, 5*time.Second, 10*time.Millisecond)
			if cancelRun {
				cancel()
				require.Error(t, process.Wait())
			} else {
				require.NoError(t, process.Wait())
			}
			cleanup()
			// systemd kills the external process; a zombie has already exited.
			assert.Eventually(t, func() bool {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
			}, 5*time.Second, 10*time.Millisecond, "detached child survived cleanup")
		})
	}
}
