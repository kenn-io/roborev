//go:build integration && linux

package sandbox

import (
	"context"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
	"golang.org/x/sys/unix"

	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}

// shellChecks prints OK or BAD for each allow/deny probe and records any failure.
const shellChecks = `
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
`

func routeJob(t *testing.T, work string) Job {
	return Job{
		WorkDir: work, MemoryMax: 1 << 30, Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()},
		Route: Route{Name: "codex", Upstream: "https://chatgpt.com/backend-api/codex", Path: "/responses", EnvVar: "CODEX_PHANTOM", Capture: []string{"/bin/echo", "fixture-token"}},
	}
}

func runInJob(t *testing.T, job Job, argv ...string) string {
	t.Helper()
	cmd, env, cleanup, err := job.Command(t.Context(), argv)
	require.NoError(t, err)
	defer cleanup()
	process := exec.CommandContext(t.Context(), cmd[0], cmd[1:]...)
	process.Env, process.Dir = env, job.WorkDir
	out, err := process.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return string(out)
}

func TestSandboxSelfCheck(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	upstream := httptest.NewTLSServer(http.NotFoundHandler())
	defer upstream.Close()
	cert := filepath.Join(t.TempDir(), "upstream.pem")
	require.NoError(t, os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600))
	job := Job{WorkDir: t.TempDir(), Route: Route{Upstream: upstream.URL + "/responses"}}
	runtimeDir := t.TempDir()
	bus, err := net.Listen("unix", filepath.Join(runtimeDir, "bus"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bus.Close() })
	for _, tc := range []struct {
		name, parent string
		runtimeEnv   []string
		want         string
	}{
		{"parent-differs", t.TempDir(), []string{"XDG_RUNTIME_DIR=" + runtimeDir}, "dbus-connect"},
		{"last-value", "", []string{"XDG_RUNTIME_DIR=" + t.TempDir(), "XDG_RUNTIME_DIR=" + runtimeDir}, "dbus-connect"},
		{"missing", runtimeDir, nil, "XDG_RUNTIME_DIR"},
		{"empty-last-value", runtimeDir, []string{"XDG_RUNTIME_DIR=" + runtimeDir, "XDG_RUNTIME_DIR="}, "XDG_RUNTIME_DIR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.parent)
			if tc.parent == "" {
				require.NoError(t, os.Unsetenv("XDG_RUNTIME_DIR"))
			}
			env := append([]string{"PATH=/usr/bin:/bin", "SSL_CERT_FILE=" + cert}, tc.runtimeEnv...)
			err := job.check(t.Context(), []string{"env"}, env, nil)
			require.ErrorContains(t, err, "sandbox self-check failed")
			assert.Contains(t, err.Error(), tc.want)
			if tc.name == "parent-differs" {
				for _, name := range []string{"canary-read", "loopback-connect", "environment-canary", "proxy-upstream"} {
					assert.Contains(t, err.Error(), name)
				}
			}
		})
	}
	work, secrets := t.TempDir(), t.TempDir()
	hostHome := filepath.Join(secrets, "codex")
	require.NoError(t, os.Mkdir(hostHome, 0o700))
	auth := filepath.Join(hostHome, "auth.json")
	require.NoError(t, os.WriteFile(auth, []byte("synthetic-token"), 0o600))
	t.Chdir(secrets)
	for _, tc := range []struct{ name, read, deny, tool, script, want string }{
		{"home", hostHome, auth, "", "", "deny-path-read"},
		{"relative", hostHome, filepath.Join("codex", "auth.json"), "", "", "deny-path-read"},
		{"repo-cat", hostHome, auth, "cat", "#!/bin/sh\nexit 1\n", "deny-path-read"},
		{"repo-bash", hostHome, auth, "bash", "#!/bin/sh\nexit 0\n", "deny-path-read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := routeJob(t, work)
			if tc.tool != "" {
				require.NoError(t, os.WriteFile(filepath.Join(work, tc.tool), []byte(tc.script), 0o700))
				job.Env[0] = "PATH=" + work + ":/usr/bin:/bin"
				defer os.Remove(filepath.Join(work, tc.tool))
			}
			job.ReadPaths, job.DenyReadPaths = []string{tc.read}, []string{tc.deny}
			_, _, cleanup, err := job.Command(t.Context(), []string{"true"})
			if cleanup != nil {
				t.Cleanup(cleanup)
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSandboxLiteralGrants(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	base := t.TempDir()
	work, read := filepath.Join(base, "job*"), filepath.Join(base, "read*")
	sibling := filepath.Join(base, "job-private")
	secret := filepath.Join(base, "read-secret")
	for _, dir := range []string{work, read, sibling} {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	readFile := filepath.Join(base, "single-file")
	require.NoError(t, os.WriteFile(readFile, []byte("allowed-file"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(read, "allowed"), []byte("allowed"), 0o600))
	home := t.TempDir()
	configDir := filepath.Join(work, "config")
	profiles := filepath.Join(configDir, "nono", "profiles")
	require.NoError(t, os.MkdirAll(profiles, 0o700))
	planted, err := json.Marshal(map[string]any{"filesystem": map[string]any{"read_file": []string{filepath.Join(home, "auth.json")}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(profiles, "default.json"), planted, 0o600))
	repo, gitDir := filepath.Join(work, "repo"), filepath.Join(read, "repo.git")
	testutil.InitSeparateGitRepo(t, repo, gitDir)
	authFile := filepath.Join(home, "auth.json")
	require.NoError(t, os.WriteFile(authFile, []byte("fixture-token"), 0o600))
	job := Job{
		WorkDir: work, ReadPaths: []string{read, readFile}, Route: Route{Name: "codex", Upstream: "https://chatgpt.com/backend-api/codex", Path: "/responses", EnvVar: "CODEX_PHANTOM", Capture: []string{"/bin/cat", authFile}},
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_CONFIG_HOME=" + configDir}, MemoryMax: 1 << 30,
	}
	script := shellChecks + `
check read-grant allow cat "$2/allowed"
check file-read allow cat "$7"
check sibling-write deny touch "$3/overwritten"
check sibling-read deny cat "$4"
check shared-tmp-write deny bash -c 'p=/tmp/roborev-probe-$$; touch "$p" && rm "$p"'
cd "$5" || exit 1
check git-read allow git status --porcelain
check git-commit deny bash -c 'echo changed >> main.go && git add main.go && git -c user.name=Test -c user.email=test@example.com commit -m probe'
check direct-upstream deny curl --fail-with-body -sS -o "$1/direct-upstream-response" https://chatgpt.com/backend-api/codex/responses
check auth-read deny cat "$6"
# Exercise the endpoint-only route beside /codex; DNS failure isn't a denial.
base=$(dirname "$CODEX_BASE_URL")
curl --fail-with-body -sS -H "Proxy-Authorization: Bearer $NONO_PROXY_TOKEN" -o "$1/direct-response" "$base/_ep_chatgpt.com/nono-no-direct-access"
[ "$?" = 22 ] || failed=1
exit "$failed"
`
	runInJob(t, job, "bash", "-c", script, "probe", work, read, sibling, secret, repo, authFile, readFile)
	assert.NoFileExists(t, filepath.Join(sibling, "overwritten"))
	denial, err := os.ReadFile(filepath.Join(work, "direct-upstream-response"))
	require.NoError(t, err)
	// nono's TLS interceptor returns this body for an endpoint-policy denial.
	assert.JSONEq(t, `{"error":"Forbidden"}`, string(denial))
	denial, err = os.ReadFile(filepath.Join(work, "direct-response"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"Forbidden"}`, string(denial))
}

func TestSandboxHostPTYIsolation(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	require.NoError(t, err)
	defer master.Close()
	require.NoError(t, unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0))
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	require.NoError(t, err)
	slavePath := fmt.Sprintf("/dev/pts/%d", number)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	require.NoError(t, err)
	defer slave.Close()
	_, err = master.WriteString("synthetic-host-input\n")
	require.NoError(t, err)
	script := shellChecks + `
check host-terminal-read deny python3 -c 'import os,sys; f=os.open(sys.argv[1],os.O_RDONLY|os.O_NONBLOCK); print(os.read(f,1024))' "$1"
check host-terminal-write deny bash -c 'echo sandbox-output > "$1"' probe "$1"
exit "$failed"
`
	runInJob(t, routeJob(t, t.TempDir()), "bash", "-c", script, "probe", slavePath)
}

func TestSandboxProcessCleanup(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	for _, tc := range []struct{ name, child string }{
		{"graceful", "/usr/bin/sleep 300"},
		{"ignore-term", `sh -c "trap '' TERM; exec /usr/bin/sleep 300"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			job := routeJob(t, work)
			marker := filepath.Join(work, "host-tool-ran")
			plant := ""
			if tc.name == "graceful" {
				t.Setenv("PATH", work+":"+os.Getenv("PATH"))
				tool := []byte("#!/bin/sh\nprintf '%s\\n' \"$0\" >> '" + marker + "'\nexec /usr/bin/\"${0##*/}\" \"$@\"\n")
				require.NoError(t, os.WriteFile(filepath.Join(work, "systemd-run"), tool, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(work, "cleanup-tool"), tool, 0o700))
				plant = "cp '" + filepath.Join(work, "cleanup-tool") + "' '" + filepath.Join(work, "systemctl") + "'; "
			}
			for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
				job.Env = append(job.Env, key+"="+os.Getenv(key))
				t.Setenv(key, "")
				require.NoError(t, os.Unsetenv(key))
			}
			tmp := t.TempDir()
			job.Env = append(job.Env, "TMPDIR="+tmp)
			pidFile := filepath.Join(work, "child.pid")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wrapped, env, cleanup, err := job.Command(ctx, []string{"bash", "-c", plant + "setsid " + tc.child + " </dev/null >/dev/null 2>&1 & echo $! > '" + pidFile + "'; wait"})
			require.NoError(t, err)
			defer cleanup()
			defer func() {
				stop := exec.Command("/usr/bin/systemctl", "--user", "stop", strings.TrimPrefix(wrapped[1], "--unit="))
				stop.Env = env
				_ = stop.Run()
			}()
			process := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
			process.Cancel = func() error { cleanup(); return nil }
			process.WaitDelay = time.Second // Bound waiting for the external wrapper when cleanup fails.
			process.Env, process.Dir = env, work
			require.NoError(t, process.Start())
			done := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = process.Wait()
				close(done)
			}()
			t.Cleanup(func() { <-done })
			// The external sandbox process publishes its child PID after startup.
			require.Eventually(t, func() bool { _, err := os.Stat(pidFile); return err == nil }, 5*time.Second, 10*time.Millisecond)
			data, err := os.ReadFile(pidFile)
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			require.NoError(t, err)
			sleeping := func() bool {
				line, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
				return string(line) == "/usr/bin/sleep\x00300\x00"
			}
			defer func() {
				// Clean up this test's child even when testing a broken wrapper.
				if sleeping() {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}()
			// The detached sleep must be running before cleanup.
			require.Eventually(t, sleeping, 5*time.Second, 10*time.Millisecond)
			if tc.name == "graceful" {
				files, err := os.ReadDir(tmp)
				require.NoError(t, err)
				require.NotEmpty(t, files, "nono must create temporary files before cancellation")
			}
			cancel()
			// Waiting depends on an external systemd scope stopping its processes.
			require.Eventually(t, func() bool {
				select {
				case <-done:
					return true
				default:
					return false
				}
			}, 15*time.Second, 10*time.Millisecond, "Wait did not return within 15s of cancellation")
			require.Error(t, waitErr)
			cleanup()
			if tc.name == "graceful" {
				assert.NoFileExists(t, marker, "host launch and cleanup must bypass tools in the writable job directory")
				files, err := os.ReadDir(tmp)
				require.NoError(t, err)
				assert.Empty(t, files, "nono left temporary files after cancellation")
			}
			// systemd kills the external process; a zombie has already exited.
			assert.Eventually(t, func() bool {
				stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
				return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
			}, 5*time.Second, 10*time.Millisecond, "detached child survived cleanup")
		})
	}
}
