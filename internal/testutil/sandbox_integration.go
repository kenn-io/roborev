//go:build integration

package testutil

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// InitSeparateGitRepo creates a committed fixture with Git metadata outside repo.
func InitSeparateGitRepo(t *testing.T, repo, gitDir string) {
	t.Helper()
	fixture := NewTestRepoWithCommit(t)
	out, err := exec.CommandContext(t.Context(), "git", "clone", "--bare", fixture.Root, gitDir).CombinedOutput()
	require.NoError(t, err, "%s", out)
	out, err = exec.CommandContext(t.Context(), "git", "--git-dir="+gitDir, "worktree", "add", "--detach", repo, "HEAD").CombinedOutput()
	require.NoError(t, err, "%s", out)
}

// RequireSandboxIntegration gates tests that need the pinned nono runtime.
func RequireSandboxIntegration(t *testing.T, commands ...string) {
	t.Helper()
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
	for _, command := range append([]string{"nono"}, commands...) {
		_, err := exec.LookPath(command)
		require.NoError(t, err, "%s is required", command)
	}
}

// SandboxShellChecks defines the allow/deny assertions used by sandbox probes.
const SandboxShellChecks = `
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
