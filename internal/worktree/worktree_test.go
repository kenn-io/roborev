package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %v\n%s", args, err, out)
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644), "failed to write %s", name)
}

// setupGitRepo creates a minimal git repo with one commit and returns its path.
func setupGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runTestGit(t, dir, "init")
	runTestGit(t, dir, "config", "user.email", "test@test.com")
	runTestGit(t, dir, "config", "user.name", "test")
	writeTestFile(t, dir, "hello.txt", "hello")
	runTestGit(t, dir, "add", "hello.txt")
	runTestGit(t, dir, "commit", "-m", "initial")
	return dir
}

func setupRepoAndWorktree(t *testing.T) (string, *Worktree) {
	t.Helper()
	repo := setupGitRepo(t)
	wt, err := Create(repo, "HEAD")
	require.NoError(t, err)
	return repo, wt
}

func TestCreateAndClose(t *testing.T) {
	_, wt := setupRepoAndWorktree(t)

	// Worktree dir should exist and contain the file
	_, err := os.Stat(filepath.Join(wt.Dir, "hello.txt"))
	require.NoError(t, err)

	wtDir := wt.Dir
	wt.Close()

	// After Close, the directory should be removed
	_, err = os.Stat(wtDir)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCapturePatchNoChanges(t *testing.T) {
	_, wt := setupRepoAndWorktree(t)
	defer wt.Close()

	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	require.Empty(t, patch)
}

func TestCapturePatchWithChanges(t *testing.T) {
	_, wt := setupRepoAndWorktree(t)
	defer wt.Close()

	// Modify a file and add a new one
	writeTestFile(t, wt.Dir, "hello.txt", "modified")
	writeTestFile(t, wt.Dir, "new.txt", "new file")

	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	require.NotEmpty(t, patch)
	assert.Contains(t, patch, "hello.txt", "patch should reference hello.txt")
	assert.Contains(t, patch, "new.txt", "patch should reference new.txt")
}

func TestCapturePatchCommittedChanges(t *testing.T) {
	_, wt := setupRepoAndWorktree(t)
	defer wt.Close()

	// Simulate an agent that commits changes (instead of just staging)
	writeTestFile(t, wt.Dir, "hello.txt", "committed-change")
	runTestGit(t, wt.Dir, "add", "-A")
	runTestGit(t, wt.Dir, "commit", "-m", "agent commit")

	// CapturePatch should still capture the committed changes
	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	require.NotEmpty(t, patch)
	assert.Contains(t, patch, "hello.txt", "patch should reference hello.txt")
	assert.Contains(t, patch, "committed-change", "patch should contain the committed content")
}

func TestApplyPatchEmpty(t *testing.T) {
	// Empty patch should be a no-op
	require.NoError(t, ApplyPatch("/nonexistent", ""))
}

func TestApplyPatchRoundTrip(t *testing.T) {
	repo, wt := setupRepoAndWorktree(t)

	// Make changes in worktree
	writeTestFile(t, wt.Dir, "hello.txt", "changed")
	writeTestFile(t, wt.Dir, "added.txt", "added")

	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	wt.Close()

	// Apply the patch back to the original repo
	require.NoError(t, ApplyPatch(repo, patch))

	// Verify the changes were applied
	content, err := os.ReadFile(filepath.Join(repo, "hello.txt"))
	require.NoError(t, err)
	assert.Equal(t, "changed", string(content))
	content, err = os.ReadFile(filepath.Join(repo, "added.txt"))
	require.NoError(t, err)
	assert.Equal(t, "added", string(content))
}

func TestCheckPatchClean(t *testing.T) {
	repo, wt := setupRepoAndWorktree(t)
	writeTestFile(t, wt.Dir, "hello.txt", "changed")
	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	wt.Close()

	// Check should pass on unmodified repo
	require.NoError(t, CheckPatch(repo, patch))
}

func TestCheckPatchEmpty(t *testing.T) {
	require.NoError(t, CheckPatch("/nonexistent", ""))
}

func TestCheckPatchConflict(t *testing.T) {
	repo, wt := setupRepoAndWorktree(t)

	// Create a patch that modifies hello.txt
	writeTestFile(t, wt.Dir, "hello.txt", "from-worktree")
	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	wt.Close()

	// Now modify hello.txt in the original repo to create a conflict
	writeTestFile(t, repo, "hello.txt", "conflicting-change")

	var conflictErr *PatchConflictError
	err = CheckPatch(repo, patch)
	require.Error(t, err)
	require.ErrorAs(t, err, &conflictErr)
}

func TestApplyPatchConflictFails(t *testing.T) {
	repo, wt := setupRepoAndWorktree(t)
	writeTestFile(t, wt.Dir, "hello.txt", "from-worktree")
	patch, err := wt.CapturePatch()
	require.NoError(t, err)
	wt.Close()

	// Create a conflict
	writeTestFile(t, repo, "hello.txt", "different")

	// ApplyPatch should fail
	require.Error(t, ApplyPatch(repo, patch))
}

func TestCreateEmptyRef(t *testing.T) {
	repo := setupGitRepo(t)
	_, err := Create(repo, "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "ref must not be empty")
}

func TestCreateWithSpecificSHA(t *testing.T) {
	repo := setupGitRepo(t)

	// Record the SHA of the initial commit
	initialSHA := runTestGit(t, repo, "rev-parse", "HEAD")

	// Add a second commit that changes hello.txt
	writeTestFile(t, repo, "hello.txt", "updated")
	runTestGit(t, repo, "add", "hello.txt")
	runTestGit(t, repo, "commit", "-m", "second commit")

	// HEAD now has "updated", but create worktree at the initial SHA
	wt, err := Create(repo, initialSHA)
	require.NoError(t, err, "Create with specific SHA failed: %v", err)
	defer wt.Close()

	// Worktree should have the original content, not "updated"
	content, err := os.ReadFile(filepath.Join(wt.Dir, "hello.txt"))
	require.NoError(t, err, "read hello.txt: %v", err)
	assert.Equal(t, "hello", string(content), "expected 'hello' at initial SHA, got %q", string(content))
}

func TestGitmodulesUsesFileProtocol(t *testing.T) {
	tpl := `[submodule "test"]
	path = test
	%s = %s
`
	tests := []struct {
		name     string
		key      string
		url      string
		expected bool
	}{
		{name: "file-scheme", key: "url", url: "file:///tmp/repo", expected: true},
		{name: "file-scheme-quoted", key: "url", url: `"file:///tmp/repo"`, expected: true},
		{name: "file-scheme-mixed-case-key", key: "URL", url: "file:///tmp/repo", expected: true},
		{name: "file-single-slash", key: "url", url: "file:/tmp/repo", expected: true},
		{name: "unix-absolute", key: "url", url: "/tmp/repo", expected: true},
		{name: "relative-dot", key: "url", url: "./repo", expected: true},
		{name: "relative-dotdot", key: "url", url: "../repo", expected: true},
		{name: "windows-drive-slash", key: "url", url: "C:/repo", expected: true},
		{name: "windows-drive-backslash", key: "url", url: `C:\repo`, expected: true},
		{name: "windows-unc", key: "url", url: `\\server\share\repo`, expected: true},
		{name: "https", key: "url", url: "https://example.com/repo.git", expected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gitmodulesPath := filepath.Join(t.TempDir(), ".gitmodules")
			require.NoError(t, os.WriteFile(gitmodulesPath, fmt.Appendf(nil, tpl, tc.key, tc.url), 0o644))

			usesFileProtocol, err := gitmodulesUsesFileProtocol(gitmodulesPath)
			require.NoError(t, err)
			require.Equal(t, tc.expected, usesFileProtocol)
		})
	}
}

func TestRepoUsesFileProtocolSubmodulesNested(t *testing.T) {
	tpl := `[submodule "test"]
	path = test
	url = %s
`
	dir := t.TempDir()
	writeTestFile(t, dir, ".gitmodules", string(fmt.Appendf(nil, tpl, "https://example.com/repo.git")))

	nestedPath := filepath.Join(dir, "sub", ".gitmodules")
	if err := os.MkdirAll(filepath.Dir(nestedPath), 0o755); err != nil {
		require.NoError(t, err, "mkdir nested: %v", err)
	}
	writeTestFile(t, dir, "sub/.gitmodules", string(fmt.Appendf(nil, tpl, "file:///tmp/repo")))

	usesFileProtocol, err := repoUsesFileProtocolSubmodules(dir)
	require.NoError(t, err)
	require.True(t, usesFileProtocol)
}

// skipIfChmodIneffective skips tests that rely on chmod 0000 blocking
// access. This is not effective on Windows or when running as root.
func skipIfChmodIneffective(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict access on Windows")
	}
	if isRoot() {
		t.Skip("chmod does not restrict access for root")
	}
}

// requireUnreadable verifies that path is actually unreadable after chmod,
// skipping the test if the filesystem ignores permissions.
func requireUnreadable(t *testing.T, path string) {
	t.Helper()
	_, err := os.Open(path)
	if err == nil {
		t.Skip("filesystem does not enforce chmod permissions")
	}
}

func TestFindGitmodulesPathsSkipsUnreadableNestedDir(t *testing.T) {
	skipIfChmodIneffective(t)

	dir := t.TempDir()
	writeTestFile(t, dir, ".gitmodules", "[submodule]\n")

	nested := filepath.Join(dir, "sub", "deep")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	writeTestFile(t, dir, "sub/deep/.gitmodules", "[submodule]\n")
	require.NoError(t, os.Chmod(nested, 0o000))
	t.Cleanup(func() { os.Chmod(nested, 0o755) })
	requireUnreadable(t, nested)

	paths, err := findGitmodulesPaths(dir)
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(dir, ".gitmodules")}, paths)
}

func TestFindGitmodulesPathsErrorsOnUnreadableRoot(t *testing.T) {
	skipIfChmodIneffective(t)

	dir := t.TempDir()
	inner := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(inner, 0o755))
	require.NoError(t, os.Chmod(inner, 0o000))
	t.Cleanup(func() { os.Chmod(inner, 0o755) })
	requireUnreadable(t, inner)

	_, err := findGitmodulesPaths(inner)
	require.Error(t, err)
}

func TestRepoUsesFileProtocolSkipsUnreadableNestedGitmodules(t *testing.T) {
	skipIfChmodIneffective(t)

	tpl := "[submodule \"test\"]\n\tpath = test\n\turl = %s\n"
	dir := t.TempDir()
	writeTestFile(t, dir, ".gitmodules",
		fmt.Sprintf(tpl, "https://example.com/repo.git"))

	nested := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	nestedFile := filepath.Join(nested, ".gitmodules")
	require.NoError(t, os.WriteFile(nestedFile,
		fmt.Appendf(nil, tpl, "file:///tmp/repo"), 0o644))
	require.NoError(t, os.Chmod(nestedFile, 0o000))
	t.Cleanup(func() { os.Chmod(nestedFile, 0o644) })
	requireUnreadable(t, nestedFile)

	usesFileProtocol, err := repoUsesFileProtocolSubmodules(dir)
	require.NoError(t, err)
	require.False(t, usesFileProtocol)
}

func TestRepoUsesFileProtocolErrorsOnUnreadableTopLevel(t *testing.T) {
	skipIfChmodIneffective(t)

	dir := t.TempDir()
	topLevel := filepath.Join(dir, ".gitmodules")
	require.NoError(t, os.WriteFile(topLevel, []byte("[submodule]\n"), 0o644))
	require.NoError(t, os.Chmod(topLevel, 0o000))
	t.Cleanup(func() { os.Chmod(topLevel, 0o644) })
	requireUnreadable(t, topLevel)

	_, err := repoUsesFileProtocolSubmodules(dir)
	require.Error(t, err)
}

func TestIsFileProtocolError(t *testing.T) {
	assert := assert.New(t)
	assert.True(isFileProtocolError(fmt.Errorf(
		"git submodule update: fatal: transport 'file' not allowed")))
	assert.True(isFileProtocolError(fmt.Errorf(
		"clone failed: transport 'file' not allowed\nretry failed")))
	assert.False(isFileProtocolError(fmt.Errorf("auth failed")))
	assert.False(isFileProtocolError(fmt.Errorf("broken URL")))
	assert.False(isFileProtocolError(nil))
}
