//go:build integration && linux

package sandbox

import (
	"context"
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
)

func TestSandboxSelfCheck(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "trust-policy.json"), []byte("invalid job policy"), 0o600))
	job := Job{
		WorkDir: work, ProxyPort: listener.Addr().(*net.TCPAddr).Port,
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, MemoryMax: 1 << 30,
	}
	marker := filepath.Join(work, "executed")
	cmd, env, cleanup, err := job.Command(context.Background(), []string{"bash", "-c", "touch '" + marker + "'"})
	require.NoError(t, err)
	defer cleanup()
	process := exec.CommandContext(t.Context(), cmd[0], cmd[1:]...)
	process.Env, process.Dir = env, work
	out, err := process.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.FileExists(t, marker)
}

func TestSandboxLiteralGrants(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
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
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			job := Job{WorkDir: work, ReadPaths: []string{read}, ProxyPort: listener.Addr().(*net.TCPAddr).Port,
				Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, MemoryMax: 1 << 30}
			script := `touch "$1/allowed" && cat "$2/allowed" && ! touch "$3/overwritten" && ! cat "$4"`
			cmd, env, cleanup, err := job.Command(t.Context(), []string{"bash", "-c", script, "probe", work, read, sibling, secret})
			require.NoError(t, err)
			defer cleanup()
			process := exec.CommandContext(t.Context(), cmd[0], cmd[1:]...)
			process.Env, process.Dir = env, work
			out, err := process.CombinedOutput()
			require.NoError(t, err, "%s", out)
			assert.NoFileExists(t, filepath.Join(sibling, "overwritten"))
		})
	}
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
