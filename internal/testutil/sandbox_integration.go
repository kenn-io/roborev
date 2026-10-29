//go:build integration

package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// InitSeparateGitRepo creates a committed fixture with Git metadata outside repo.
func InitSeparateGitRepo(t *testing.T, repo, gitDir string) {
	t.Helper()
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
