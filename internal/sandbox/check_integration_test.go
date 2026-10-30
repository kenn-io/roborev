//go:build integration && linux

package sandbox

import (
	"encoding/json/v2"
	"fmt"
	"net"
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
	job := Job{WorkDir: t.TempDir()}
	err := job.check(t.Context(), []string{"env"}, []string{"PATH=/usr/bin:/bin"})
	require.ErrorContains(t, err, "sandbox self-check failed")
	for _, name := range []string{"canary-read", "loopback-connect", "environment-canary"} {
		assert.Contains(t, err.Error(), name)
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
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer other.Close()
	job := Job{
		WorkDir: work, ReadPaths: []string{read}, Route: Route{Name: "codex", Upstream: "https://chatgpt.com/backend-api/codex", Path: "/responses", EnvVar: "CODEX_PHANTOM", Capture: []string{"/bin/cat", authFile}},
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_CONFIG_HOME=" + configDir}, MemoryMax: 1 << 30,
	}
	script := shellChecks + `
check work-write allow touch "$1/allowed"
check read-grant allow cat "$2/allowed"
check sibling-write deny touch "$3/overwritten"
check sibling-read deny cat "$4"
check home-write deny touch "$HOME/write"
check shared-tmp-write deny bash -c 'p=/tmp/roborev-probe-$$; touch "$p" && rm "$p"'
cd "$5" || exit 1
check git-read allow git status --porcelain
check git-commit deny bash -c 'echo changed >> main.go && git add main.go && git -c user.name=Test -c user.email=test@example.com commit -m probe'
check git-config deny git config --local sandbox.probe changed
check dbus-systemd deny env XDG_RUNTIME_DIR="$6" DBUS_SESSION_BUS_ADDRESS="unix:path=$6/bus" systemd-run --user --wait -q /usr/bin/true
check external-domain deny curl -fsS https://example.com
check other-loopback deny bash -c ': >/dev/tcp/127.0.0.1/"$1"' probe "$7"
check direct-upstream deny curl --fail-with-body -sS -o "$1/direct-upstream-response" https://chatgpt.com/backend-api/codex/responses
check auth-read deny cat "$8"
# Exercise the endpoint-only route beside /codex; DNS failure isn't a denial.
base=$(dirname "$CODEX_BASE_URL")
curl --fail-with-body -sS -H "Proxy-Authorization: Bearer $NONO_PROXY_TOKEN" -o "$1/direct-response" "$base/_ep_chatgpt.com/nono-no-direct-access"
[ "$?" = 22 ] || failed=1
exit "$failed"
`
	out := runInJob(t, job, "bash", "-c", script, "probe", work, read, sibling, secret, repo, os.Getenv("XDG_RUNTIME_DIR"), strconv.Itoa(other.Addr().(*net.TCPAddr).Port), authFile)
	for _, name := range []string{"work-write", "read-grant", "sibling-write", "sibling-read", "home-write", "shared-tmp-write", "git-read", "git-commit", "git-config", "dbus-systemd", "external-domain", "other-loopback", "direct-upstream", "auth-read"} {
		assert.Contains(t, out, "OK "+name)
	}
	assert.NoFileExists(t, filepath.Join(sibling, "overwritten"))
	denial, err := os.ReadFile(filepath.Join(work, "direct-upstream-response"))
	require.NoError(t, err)
	// nono's TLS interceptor returns this body for an endpoint-policy denial.
	assert.JSONEq(t, `{"error":"Forbidden"}`, string(denial))
	denial, err = os.ReadFile(filepath.Join(work, "direct-response"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"error":"Forbidden"}`, string(denial))
}

func TestSandboxUnenforceablePolicy(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	job := routeJob(t, t.TempDir())
	job.Route.Upstream = "http://example.com"
	_, _, _, err := job.Command(t.Context(), []string{"true"})
	require.ErrorContains(t, err, "must use HTTPS (HTTP only allowed for loopback addresses)")
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
	out := runInJob(t, routeJob(t, t.TempDir()), "bash", "-c", script, "probe", slavePath)
	assert.Contains(t, out, "OK host-terminal-read")
	assert.Contains(t, out, "OK host-terminal-write")
}

func TestSandboxProcessCleanup(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	work := t.TempDir()
	job := routeJob(t, work)
	pidFile := filepath.Join(work, "child.pid")
	wrapped, env, cleanup, err := job.Command(t.Context(), []string{"bash", "-c", "setsid /usr/bin/sleep 300 </dev/null >/dev/null 2>&1 & echo $! > '" + pidFile + "'"})
	require.NoError(t, err)
	defer cleanup()
	process := exec.CommandContext(t.Context(), wrapped[0], wrapped[1:]...)
	process.Env, process.Dir = env, work
	require.NoError(t, process.Run())
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
	cleanup()
	// systemd kills the external process; a zombie has already exited.
	assert.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
	}, 5*time.Second, 10*time.Millisecond, "detached child survived cleanup")
}
