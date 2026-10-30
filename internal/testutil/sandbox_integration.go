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
func RequireSandboxIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("ROBOREV_RUN_SANDBOX_INTEGRATION") != "1" || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires opt-in linux/amd64 sandbox integration")
	}
}
