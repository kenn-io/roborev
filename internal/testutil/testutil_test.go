package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func findBaselineCommand(t *testing.T) string {
	t.Helper()
	// Find a command that exists before isolation so we can
	// verify it becomes unreachable afterward. Try several
	// candidates to handle both Unix and Windows.
	candidates := []string{"ls", "cat", "echo", "cmd", "whoami"}
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	t.Skip("no baseline command found in PATH; cannot verify isolation")
	return ""
}

func TestMockExecutableIsolated(t *testing.T) {
	baseline := findBaselineCommand(t)

	cleanup := MockExecutableIsolated(t, "my-mock-tool", 0)
	defer cleanup()

	_, err := exec.LookPath("my-mock-tool")
	require.NoError(t, err, "expected to find my-mock-tool in PATH")

	_, err = exec.LookPath(baseline)
	require.Error(t, err, "expected %q to be absent from isolated PATH", baseline)
}

func TestMockBinaryHelpersRestorePath(t *testing.T) {
	origPath := os.Getenv("PATH")

	cleanup := MockBinaryInPath(t, "test-script", "#!/bin/sh\nexit 0\n")

	_, err := exec.LookPath("test-script")
	require.NoError(t, err, "expected mocked binary in PATH")

	cleanup()

	require.Equal(t, origPath, os.Getenv("PATH"))
}

func TestCommitFileCreatesParentDirectories(t *testing.T) {
	repo := InitTestRepo(t)

	sha := repo.CommitFile(filepath.Join("nested", "dir", "file.txt"), "hello", "add nested file")

	require.Equal(t, repo.HeadSHA(), sha)

	content, err := os.ReadFile(filepath.Join(repo.Root, "nested", "dir", "file.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
}

func TestGitRepoHelpersDisableAutoMaintenance(t *testing.T) {
	repo := InitTestRepo(t)

	require.Equal(t, "0", repo.Run("config", "--get", "gc.auto"))
	require.Equal(t, "false", repo.Run("config", "--get", "maintenance.auto"))

	dir := filepath.Join(t.TempDir(), "repo")
	InitTestGitRepo(t, dir)

	require.Equal(t, "0", runGit(t, dir, nil, "config", "--get", "gc.auto"))
	require.Equal(t, "false", runGit(t, dir, nil, "config", "--get", "maintenance.auto"))
}

func TestNewGitRepoReturnsResolvedPath(t *testing.T) {
	repo := NewGitRepo(t)

	resolved, err := filepath.EvalSymlinks(repo.Root)
	require.NoError(t, err)
	require.Equal(t, resolved, repo.Path())
}
