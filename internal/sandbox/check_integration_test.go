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
	"go.kenn.io/roborev/internal/testutil"
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
	for _, name := range []string{"canary-read", "loopback-connect", "environment-canary"} {
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
	job := Job{WorkDir: work, ReadPaths: []string{read}, Route: Route{Name: "codex", Upstream: "https://chatgpt.com/backend-api/codex", Path: "/responses", EnvVar: "CODEX_PHANTOM", Capture: []string{"/bin/cat", authFile}},
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_CONFIG_HOME=" + configDir}, MemoryMax: 1 << 30}
	script := testutil.SandboxShellChecks + `
check work-write allow touch "$1/allowed"
check read-grant allow cat "$2/allowed"
check sibling-write deny touch "$3/overwritten"
check sibling-read deny cat "$4"
check home-write deny touch "$HOME/write"
check shared-tmp-write deny bash -c 'p=/tmp/roborev-probe-$$; touch "$p" && rm "$p"'
cd "$5" || exit 1
check git-read allow git status --porcelain
check git-commit deny bash -c 'echo changed >> README.md && git add README.md && git -c user.name=Test -c user.email=test@example.com commit -m probe'
check git-config deny git config --local sandbox.probe changed
check dbus-systemd deny env XDG_RUNTIME_DIR="$6" DBUS_SESSION_BUS_ADDRESS="unix:path=$6/bus" systemd-run --user --wait -q /usr/bin/true
check external-domain deny curl -fsS https://example.com
check other-loopback deny bash -c ': >/dev/tcp/127.0.0.1/"$1"' probe "$7"
check direct-upstream deny curl --fail-with-body -sS -o "$1/direct-upstream-response" https://chatgpt.com/backend-api/codex/responses
check route-get deny bash -c 'curl -fsS "$CODEX_BASE_URL/other" -H "Authorization: Bearer $CODEX_PHANTOM"'
check route-no-key deny bash -c 'curl -fsS -X POST "$CODEX_BASE_URL/responses"'
check route-wrong-key deny bash -c 'curl -fsS -X POST "$CODEX_BASE_URL/responses" -H "Authorization: Bearer wrong-key"'
check auth-read deny cat "$8"
exit "$failed"
`
	cmd, env, cleanup, err := job.Command(t.Context(), []string{"bash", "-c", script, "probe", work, read, sibling, secret, repo, os.Getenv("XDG_RUNTIME_DIR"), strconv.Itoa(other.Addr().(*net.TCPAddr).Port), authFile})
	require.NoError(t, err)
	defer cleanup()
	process := exec.CommandContext(t.Context(), cmd[0], cmd[1:]...)
	process.Env, process.Dir = env, work
	out, err := process.CombinedOutput()
	require.NoError(t, err, "%s", out)
	assert.NotContains(t, string(out), "BAD ")
	for _, name := range []string{"work-write", "read-grant", "sibling-write", "sibling-read", "home-write", "shared-tmp-write", "git-read", "git-commit", "git-config", "dbus-systemd", "external-domain", "other-loopback", "direct-upstream", "route-get", "route-no-key", "route-wrong-key", "auth-read"} {
		assert.Contains(t, string(out), "OK "+name)
	}
	assert.NoFileExists(t, filepath.Join(sibling, "overwritten"))
	denial, err := os.ReadFile(filepath.Join(work, "direct-upstream-response"))
	require.NoError(t, err)
	// nono's TLS interceptor returns this body for an endpoint-policy denial.
	assert.Equal(t, `{"error":"Forbidden"}`, string(denial))
}

func TestSandboxUnenforceablePolicy(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	job := Job{WorkDir: t.TempDir(), Route: Route{Name: "codex", Upstream: "http://example.com", Path: "/responses", EnvVar: "CODEX_PHANTOM", Capture: []string{"/bin/echo", "fixture-token"}}, MemoryMax: 1 << 30,
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}}
	_, _, _, err := job.Command(t.Context(), []string{"true"})
	require.ErrorContains(t, err, "must use HTTPS (HTTP only allowed for loopback addresses)")
}

func TestSandboxProcessCleanup(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	if _, err := exec.LookPath("nono"); err != nil {
		t.Skip("nono is required")
	}
	work := t.TempDir()
	job := Job{
		WorkDir: work,
		Route:   Route{Name: "codex", Upstream: "https://chatgpt.com/backend-api/codex", Path: "/responses", EnvVar: "CODEX_PHANTOM", Capture: []string{"/bin/echo", "fixture-token"}},
		Env:     []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, MemoryMax: 1 << 30,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	script := `setsid /usr/bin/sleep 300 </dev/null >/dev/null 2>&1 & echo $! > '` + filepath.Join(work, "child.pid") + `'`
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
	// The external shell must start the detached sleep before cleanup.
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
	require.NoError(t, process.Wait())
	cleanup()
	// systemd kills the external process; a zombie has already exited.
	assert.Eventually(t, func() bool {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		return os.IsNotExist(err) || strings.Contains(string(stat), ") Z ")
	}, 5*time.Second, 10*time.Millisecond, "detached child survived cleanup")
}
