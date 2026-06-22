package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

func TestMain(m *testing.M) {
	if os.Getenv("ROBOREV_GIT_HOOK_PWD_HELPER") == "1" {
		os.Exit(runGitHookPWDHelper())
	}
	os.Exit(testenv.RunIsolatedMain(m))
}

func runGitHookPWDHelper() int {
	cwd, err := os.Getwd()
	if err != nil {
		_, _ = os.Stderr.WriteString("get cwd: " + err.Error() + "\n")
		return 1
	}
	pwd := os.Getenv("PWD")
	pwd = cleanHookPWDPath(pwd)
	cwd = cleanHookPWDPath(cwd)
	if pwd != cwd {
		_, _ = os.Stderr.WriteString("PWD mismatch: " + pwd + " != " + cwd + "\n")
		return 1
	}
	return 0
}

func cleanHookPWDPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

type TestRepo struct {
	T   *testing.T
	Dir string
}

func NewTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	return NewTestRepoWithAuthor(t, "Test")
}

func NewTestRepoWithAuthor(t *testing.T, author string) *TestRepo {
	t.Helper()
	dir := t.TempDir()
	r := &TestRepo{T: t, Dir: dir}
	r.Run("init")
	r.Run("config", "user.email", "test@test.com")
	r.Run("config", "user.name", author)
	return r
}

func NewTestRepoWithCommit(t *testing.T) *TestRepo {
	t.Helper()
	repo := NewTestRepo(t)
	repo.CommitFile("initial.txt", "initial content", "initial commit")
	return repo
}

func NewBareTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	dir := t.TempDir()
	r := &TestRepo{T: t, Dir: dir}
	r.Run("init", "--bare")
	return r
}

func (r *TestRepo) Run(args ...string) string {
	r.T.Helper()
	return runGit(r.T, r.Dir, args...)
}

func TestIsTransientGitError(t *testing.T) {
	assert := assert.New(t)
	// The exact MSYS sh.exe fork failure observed on Windows CI.
	winFork := "0 [main] sh (4236) C:\\Program Files\\Git\\usr\\bin\\sh.exe: " +
		"*** fatal error - add_item (\"\\??\\C:\\Program Files\\Git\", \"/\", ...) " +
		"failed, errno 1\nfatal: Could not read from remote repository."
	transient := []string{
		winFork,
		"git: fork: retry: Resource temporarily unavailable",
		"error: cannot fork() for fetch-pack",
		"DLL initialization failed",
	}
	for _, out := range transient {
		assert.True(isTransientGitError([]byte(out)), "should retry: %q", out)
	}
	durable := []string{
		"",
		"CONFLICT (content): Merge conflict in file.txt",
		"nothing to commit, working tree clean",
		"fatal: not a git repository",
		// A generic remote-read failure without the MSYS fork signature must
		// not be retried -- it is a real, durable error.
		"fatal: Could not read from remote repository.",
	}
	for _, out := range durable {
		assert.False(isTransientGitError([]byte(out)), "should not retry: %q", out)
	}
}

func (r *TestRepo) CommitFile(filename, content, msg string) {
	r.T.Helper()
	r.WriteFile(filename, content)
	r.Run("add", filename)
	r.Run("commit", "-m", msg)
}

func (r *TestRepo) CommitAll(msg string) {
	r.T.Helper()
	r.Run("add", ".")
	r.Run("commit", "-m", msg)
}

func (r *TestRepo) WriteFile(filename, content string) {
	r.T.Helper()
	path := filepath.Join(r.Dir, filename)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	require.NoError(r.T, err)
	err = os.WriteFile(path, []byte(content), 0o644)
	require.NoError(r.T, err)
}

func (r *TestRepo) HeadSHA() string {
	r.T.Helper()
	return r.Run("rev-parse", "HEAD")
}

func (r *TestRepo) AddWorktree(branchName string) *TestRepo {
	r.T.Helper()
	wtDir := r.T.TempDir()
	r.Run("worktree", "add", wtDir, "-b", branchName)
	r.T.Cleanup(func() {
		cmd := exec.Command("git", "worktree", "remove", wtDir)
		cmd.Dir = r.Dir
		_ = cmd.Run()
	})
	return &TestRepo{T: r.T, Dir: wtDir}
}

func (r *TestRepo) InstallHook(name, script string) {
	r.T.Helper()
	hooksDir := filepath.Join(r.Dir, ".git", "hooks")
	err := os.MkdirAll(hooksDir, 0o755)
	require.NoError(r.T, err)
	hookPath := filepath.Join(hooksDir, name)
	err = os.WriteFile(hookPath, []byte(script), 0o755)
	require.NoError(r.T, err)
}

const (
	gitTransientRetries   = 4
	gitTransientRetryWait = 250 * time.Millisecond
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out []byte
	var err error
	for attempt := 0; ; attempt++ {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err = cmd.CombinedOutput()
		if err == nil || attempt >= gitTransientRetries || !isTransientGitError(out) {
			break
		}
		time.Sleep(gitTransientRetryWait)
	}
	require.NoError(t, err, "git %v failed: %v\n%s", args, err, out)
	return strings.TrimSpace(string(out))
}

// isTransientGitError reports whether git output matches the intermittent
// MSYS2/Cygwin process-spawn failures seen on Windows CI runners. git's
// local transport (clone/fetch/push to a filesystem path) forks sh.exe, and
// that fork sporadically aborts before doing any work -- e.g. "fatal error -
// add_item ... failed" or "Resource temporarily unavailable". The command
// performed no partial work, so retrying it is safe.
func isTransientGitError(out []byte) bool {
	s := string(out)
	for _, sig := range []string{
		"fatal error - add_item",
		"Resource temporarily unavailable",
		"fork: retry",
		"cannot fork",
		"unable to fork",
		"DLL initialization failed",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

func TestIsUnbornHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("true for empty repo", func(t *testing.T) {
		repo := NewTestRepo(t)
		assert.True(t, IsUnbornHead(repo.Dir), "expected IsUnbornHead=true for empty repo")
	})

	t.Run("false after first commit", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		assert.False(t, IsUnbornHead(repo.Dir), "expected IsUnbornHead=false after commit")
	})

	t.Run("false for non-git directory", func(t *testing.T) {
		dir := t.TempDir()
		assert.False(t, IsUnbornHead(dir), "expected IsUnbornHead=false for non-git dir")
	})

	t.Run("false for corrupt ref", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		headRef := strings.TrimSpace(repo.Run("symbolic-ref", "HEAD"))
		repo.WriteFile(filepath.Join(".git", headRef), "0000000000000000000000000000000000000000\n")

		assert.False(t, IsUnbornHead(repo.Dir), "expected IsUnbornHead=false for corrupt ref (ref exists but object is missing)")
	})
}

func TestNormalizeMSYSPath(t *testing.T) {
	expectedCUsers := "/c/Users/test"
	expectedCapCUsers := "/C/Users/test"
	expectedUnix := "/home/user/repo"

	if runtime.GOOS == "windows" {
		expectedCUsers = "C:\\Users\\test"
		expectedCapCUsers = "C:\\Users\\test"
		expectedUnix = "\\home\\user\\repo"
	}

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"forward slash path", "C:/Users/test", "C:" + string(filepath.Separator) + "Users" + string(filepath.Separator) + "test"},
		{"MSYS lowercase drive", "/c/Users/test", expectedCUsers},
		{"MSYS uppercase drive", "/C/Users/test", expectedCapCUsers},
		{"Unix absolute path", "/home/user/repo", expectedUnix},
		{"relative path", "some/path", "some" + string(filepath.Separator) + "path"},
		{"with trailing newline", "C:/Users/test\n", "C:" + string(filepath.Separator) + "Users" + string(filepath.Separator) + "test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeMSYSPath(tt.input)
			assert.Equal(t, tt.expected, result, "Expected %s, got %s", tt.expected, result)
		})
	}
}

func TestGetHooksPath(t *testing.T) {
	t.Run("default hooks path", func(t *testing.T) {
		repo := NewTestRepo(t)

		hooksPath, err := GetHooksPath(repo.Dir)
		require.NoError(t, err, "GetHooksPath failed: %v", err)
		assert.True(t, filepath.IsAbs(hooksPath),
			"hooks path should be absolute, got: %s", hooksPath)

		cleanPath := filepath.Clean(hooksPath)
		expectedSuffix := filepath.Join(".git", "hooks")
		assert.True(t, strings.HasSuffix(cleanPath, expectedSuffix),
			"hooks path should end with %s, got: %s",
			expectedSuffix, cleanPath)
	})

	t.Run("custom core.hooksPath absolute", func(t *testing.T) {
		repo := NewTestRepo(t)
		customHooksDir := filepath.Join(repo.Dir, "my-hooks")
		err := os.MkdirAll(customHooksDir, 0o755)
		require.NoError(t, err)

		repo.Run("config", "core.hooksPath", customHooksDir)

		hooksPath, err := GetHooksPath(repo.Dir)
		require.NoError(t, err, "GetHooksPath failed: %v", err)

		assert.Equal(t, customHooksDir, hooksPath, "expected hooksPath=%s, got %s", customHooksDir, hooksPath)
	})

	t.Run("custom core.hooksPath relative", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("config", "core.hooksPath", "custom-hooks")

		hooksPath, err := GetHooksPath(repo.Dir)
		require.NoError(t, err, "GetHooksPath failed: %v", err)

		assert.True(t, filepath.IsAbs(hooksPath),
			"hooks path should be absolute, got: %s", hooksPath)
		// GetMainRepoRoot resolves symlinks, so compare
		// against the resolved dir.
		resolvedDir, err := filepath.EvalSymlinks(repo.Dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(resolvedDir, "custom-hooks"),
			hooksPath)
	})

	t.Run("relative hooksPath resolves to main repo from worktree",
		func(t *testing.T) {
			repo := NewTestRepo(t)
			repo.Run("commit", "--allow-empty", "-m", "init")
			repo.Run("config", "core.hooksPath", ".githooks")

			wtDir := t.TempDir()
			resolved, err := filepath.EvalSymlinks(wtDir)
			require.NoError(t, err)
			repo.Run("worktree", "add", resolved, "-b", "wt")

			hooksPath, err := GetHooksPath(resolved)
			require.NoError(t, err)

			resolvedMain, err := filepath.EvalSymlinks(repo.Dir)
			require.NoError(t, err)
			assert.Equal(t,
				filepath.Join(resolvedMain, ".githooks"),
				hooksPath,
				"should resolve against main repo, not worktree",
			)
		})

	t.Run("default hooksPath resolves to main repo from worktree",
		func(t *testing.T) {
			repo := NewTestRepo(t)
			repo.Run("commit", "--allow-empty", "-m", "init")

			wtDir := t.TempDir()
			resolved, err := filepath.EvalSymlinks(wtDir)
			require.NoError(t, err)
			repo.Run("worktree", "add", resolved, "-b", "wt")

			hooksPath, err := GetHooksPath(resolved)
			require.NoError(t, err)

			resolvedMain, err := filepath.EvalSymlinks(repo.Dir)
			require.NoError(t, err)
			assert.Equal(t,
				filepath.Join(resolvedMain, ".git", "hooks"),
				hooksPath,
				"default hooks path from worktree should point "+
					"at main repo .git/hooks",
			)
		})
}

func TestEnsureAbsoluteHooksPath(t *testing.T) {
	t.Run("noop when not set", func(t *testing.T) {
		repo := NewTestRepo(t)
		err := EnsureAbsoluteHooksPath(repo.Dir)
		require.NoError(t, err)

		// Verify core.hooksPath is still unset.
		cmd := exec.Command(
			"git", "config", "--local", "core.hooksPath",
		)
		cmd.Dir = repo.Dir
		assert.Error(t, cmd.Run(), "core.hooksPath should remain unset")
	})

	t.Run("noop when already absolute", func(t *testing.T) {
		repo := NewTestRepo(t)
		absPath := filepath.Join(repo.Dir, "my-hooks")
		repo.Run("config", "core.hooksPath", absPath)

		err := EnsureAbsoluteHooksPath(repo.Dir)
		require.NoError(t, err)

		got := repo.Run("config", "--local", "core.hooksPath")
		assert.Equal(t, absPath, got)
	})

	t.Run("noop for tilde home path", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("config", "core.hooksPath", "~/my-hooks")

		err := EnsureAbsoluteHooksPath(repo.Dir)
		require.NoError(t, err)

		got := repo.Run("config", "--local", "core.hooksPath")
		assert.Equal(t, "~/my-hooks", got,
			"~/path should be left for git to expand")
	})

	t.Run("noop for bare tilde", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("config", "core.hooksPath", "~")

		err := EnsureAbsoluteHooksPath(repo.Dir)
		require.NoError(t, err)

		got := repo.Run("config", "--local", "core.hooksPath")
		assert.Equal(t, "~", got,
			"bare ~ should be left for git to expand")
	})

	t.Run("converts relative to absolute", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("config", "core.hooksPath", ".githooks")

		err := EnsureAbsoluteHooksPath(repo.Dir)
		require.NoError(t, err)

		got := repo.Run("config", "--local", "core.hooksPath")
		assert.True(t, filepath.IsAbs(got),
			"expected absolute path, got: %s", got)
		// GetMainRepoRoot resolves symlinks (e.g. macOS
		// /var → /private/var), so compare against the
		// resolved repo dir.
		resolvedDir, err := filepath.EvalSymlinks(repo.Dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(resolvedDir, ".githooks"), got)
	})

	t.Run("resolves against main repo root from worktree", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("commit", "--allow-empty", "-m", "init")
		repo.Run("config", "core.hooksPath", ".githooks")

		wtDir := t.TempDir()
		resolved, err := filepath.EvalSymlinks(wtDir)
		require.NoError(t, err)
		repo.Run("worktree", "add", resolved, "-b", "wt-branch")

		// Run from the linked worktree, not the main repo.
		err = EnsureAbsoluteHooksPath(resolved)
		require.NoError(t, err)

		// The rewritten path must point at the main repo's
		// .githooks, not the worktree's.
		resolvedMain, err := filepath.EvalSymlinks(repo.Dir)
		require.NoError(t, err)
		wt := &TestRepo{T: t, Dir: resolved}
		got := wt.Run("config", "--local", "core.hooksPath")
		assert.Equal(t, filepath.Join(resolvedMain, ".githooks"), got,
			"should resolve against main repo root, not worktree")
	})

	t.Run("overrides relative global config with local absolute",
		func(t *testing.T) {
			// Simulate a global ~/.gitconfig with relative
			// core.hooksPath (no local config set).
			fakeHome := t.TempDir()
			t.Setenv("HOME", fakeHome)
			t.Setenv("USERPROFILE", fakeHome)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(
				fakeHome, ".config",
			))
			globalCfg := filepath.Join(fakeHome, ".gitconfig")
			t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
			err := os.WriteFile(globalCfg, []byte(
				"[core]\n\thooksPath = .githooks\n",
			), 0o644)
			require.NoError(t, err)

			repo := NewTestRepo(t)

			// Verify no local override exists yet.
			check := exec.Command(
				"git", "config", "--local", "core.hooksPath",
			)
			check.Dir = repo.Dir
			require.Error(t, check.Run(),
				"should have no local core.hooksPath")

			err = EnsureAbsoluteHooksPath(repo.Dir)
			require.NoError(t, err)

			// Should now have a local absolute override.
			got := repo.Run(
				"config", "--local", "core.hooksPath",
			)
			assert.True(t, filepath.IsAbs(got),
				"expected absolute path, got: %s", got)
			resolvedDir, err := filepath.EvalSymlinks(repo.Dir)
			require.NoError(t, err)
			assert.Equal(t,
				filepath.Join(resolvedDir, ".githooks"), got,
			)
		})
}

func TestIsRebaseInProgress(t *testing.T) {
	t.Run("no rebase", func(t *testing.T) {
		repo := NewTestRepo(t)
		assert.False(t, IsRebaseInProgress(repo.Dir), "expected no rebase in progress")
	})

	t.Run("rebase-merge directory", func(t *testing.T) {
		repo := NewTestRepo(t)
		rebaseMerge := filepath.Join(repo.Dir, ".git", "rebase-merge")
		err := os.MkdirAll(rebaseMerge, 0o755)
		require.NoError(t, err)
		assert.True(t, IsRebaseInProgress(repo.Dir), "should detect rebase-merge")
	})

	t.Run("rebase-apply directory", func(t *testing.T) {
		repo := NewTestRepo(t)
		rebaseApply := filepath.Join(repo.Dir, ".git", "rebase-apply")
		err := os.MkdirAll(rebaseApply, 0o755)
		require.NoError(t, err)
		assert.True(t, IsRebaseInProgress(repo.Dir), "should detect rebase-apply")
	})

	t.Run("non-repo returns false", func(t *testing.T) {
		nonRepo := t.TempDir()
		assert.False(t, IsRebaseInProgress(nonRepo), "non-repo should not be in rebase")
	})

	t.Run("worktree with rebase", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		wt := repo.AddWorktree("test-branch")

		gitPath := filepath.Join(wt.Dir, ".git")
		info, err := os.Stat(gitPath)
		require.NoError(t, err, "worktree .git not found: %v", err)
		if info.IsDir() {
			t.Skip("worktree has .git directory instead of file - older git version")
		}
		require.False(t, IsRebaseInProgress(wt.Dir), "worktree should not be in rebase")

		worktreeGitDir := strings.TrimSpace(wt.Run("rev-parse", "--git-dir"))
		if !filepath.IsAbs(worktreeGitDir) {
			worktreeGitDir = filepath.Join(wt.Dir, worktreeGitDir)
		}

		rebaseMerge := filepath.Join(worktreeGitDir, "rebase-merge")
		err = os.MkdirAll(rebaseMerge, 0o755)
		require.NoError(t, err)
		require.True(t, IsRebaseInProgress(wt.Dir), "worktree should detect rebase")
	})
}

func TestGetMainRepoRootForBareBackedWorktree(t *testing.T) {
	bareRepo := NewBareTestRepo(t)
	seedRepo := NewTestRepoWithCommit(t)
	seedRepo.Run("remote", "add", "origin", bareRepo.Dir)
	seedRepo.Run("push", "origin", "HEAD:main")

	worktreeDir := t.TempDir()
	bareRepo.Run("worktree", "add", worktreeDir, "main")
	t.Cleanup(func() {
		cmd := exec.Command("git", "-C", bareRepo.Dir, "worktree", "remove", worktreeDir)
		_ = cmd.Run()
	})

	got, err := GetMainRepoRoot(worktreeDir)
	require.NoError(t, err)

	want, err := GetRepoRoot(worktreeDir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestGetCommitInfo(t *testing.T) {
	t.Run("commit with subject only", func(t *testing.T) {
		repo := NewTestRepoWithAuthor(t, "Test Author")

		repo.CommitFile("file1.txt", "content", "Simple subject")

		commitSHA := repo.HeadSHA()

		info, err := GetCommitInfo(repo.Dir, commitSHA)
		require.NoError(t, err, "GetCommitInfo failed: %v", err)
		assert.Equal(t, "Simple subject", info.Subject, "expected subject 'Simple subject', got '%s'", info.Subject)

		assert.Empty(t, info.Body, "expected empty body, got '%s'", info.Body)
		assert.Equal(t, "Test Author", info.Author, "expected author 'Test Author', got '%s'", info.Author)
	})

	t.Run("commit with subject and body", func(t *testing.T) {
		repo := NewTestRepoWithAuthor(t, "Test Author")
		repo.WriteFile("file2.txt", "content2")
		repo.Run("add", ".")

		commitMsg := "Subject line\n\nThis is the body.\nIt has multiple lines.\n\nAnd paragraphs."
		repo.Run("commit", "-m", commitMsg)

		commitSHA := repo.HeadSHA()

		info, err := GetCommitInfo(repo.Dir, commitSHA)
		require.NoError(t, err, "GetCommitInfo failed: %v", err)
		assert.Equal(t, "Subject line", info.Subject, "expected subject 'Subject line', got '%s'", info.Subject)

		assert.Contains(t, info.Body, "This is the body", "expected body to contain 'This is the body', got '%s'", info.Body)
		assert.Contains(t, info.Body, "multiple lines", "expected body to contain 'multiple lines', got '%s'", info.Body)
	})

	t.Run("commit with pipe in message", func(t *testing.T) {
		repo := NewTestRepoWithAuthor(t, "Test Author")
		repo.WriteFile("file3.txt", "content3")
		repo.Run("add", ".")

		commitMsg := "Fix bug | important\n\nDetails: foo | bar | baz"
		repo.Run("commit", "-m", commitMsg)

		commitSHA := repo.HeadSHA()

		info, err := GetCommitInfo(repo.Dir, commitSHA)
		require.NoError(t, err, "GetCommitInfo failed: %v", err)

		assert.Contains(t, info.Subject, "|", "expected subject to contain pipe, got '%s'", info.Subject)
		assert.Contains(t, info.Body, "foo | bar", "expected body to contain 'foo | bar', got '%s'", info.Body)
	})
}

func TestGetCommitParents(t *testing.T) {
	repo := NewTestRepo(t)
	repo.CommitFile("base.txt", "base", "base")
	baseSHA := repo.HeadSHA()
	defaultBranch := repo.Run("rev-parse", "--abbrev-ref", "HEAD")

	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("feature.txt", "feature", "feature")
	featureSHA := repo.HeadSHA()

	repo.Run("checkout", defaultBranch)
	repo.Run("merge", "--no-ff", "feature", "-m", "merge feature")
	mergeSHA := repo.HeadSHA()

	parents, err := GetCommitParents(repo.Dir, mergeSHA)
	require.NoError(t, err, "GetCommitParents failed")
	assert.Equal(t, []string{baseSHA, featureSHA}, parents)

	rootParents, err := GetCommitParents(repo.Dir, baseSHA)
	require.NoError(t, err, "GetCommitParents root failed")
	assert.Empty(t, rootParents)
}

func TestGetBranchName(t *testing.T) {
	t.Run("valid commit on branch", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		commitSHA := repo.HeadSHA()
		expectedBranch := repo.Run("rev-parse", "--abbrev-ref", "HEAD")

		branch := GetBranchName(repo.Dir, commitSHA)
		assert.Equal(t, expectedBranch, branch, "expected %s, got %s", expectedBranch, branch)
	})

	t.Run("commit behind branch head", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		commitSHA := repo.HeadSHA()
		expectedBranch := repo.Run("rev-parse", "--abbrev-ref", "HEAD")

		repo.CommitFile("file2.txt", "content2", "second")

		branch := GetBranchName(repo.Dir, commitSHA)
		assert.Equal(t, expectedBranch, branch, "expected %s (suffix stripped), got %s", expectedBranch, branch)
	})

	t.Run("non-existent repo returns empty", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		commitSHA := repo.HeadSHA()

		nonRepo := t.TempDir()
		branch := GetBranchName(nonRepo, commitSHA)
		assert.Empty(t, branch, "expected empty string, got %s", branch)
	})

	t.Run("invalid SHA returns empty", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		branch := GetBranchName(repo.Dir, "0000000000000000000000000000000000000000")
		assert.Empty(t, branch, "expected empty string, got %s", branch)
	})
}

func TestGetCurrentBranch(t *testing.T) {
	t.Run("returns current branch", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		branch := GetCurrentBranch(repo.Dir)
		assert.NotEmpty(t, branch)
		assert.NotContains(t, branch, "heads/",
			"branch should not have heads/ prefix")
	})

	t.Run("returns branch after checkout", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.Run("checkout", "-b", "feature-branch")

		branch := GetCurrentBranch(repo.Dir)
		assert.Equal(t, "feature-branch", branch)
	})

	t.Run("returns empty for detached HEAD", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		sha := repo.HeadSHA()
		repo.Run("checkout", sha)

		branch := GetCurrentBranch(repo.Dir)
		assert.Empty(t, branch)
	})

	t.Run("returns empty for non-repo", func(t *testing.T) {
		nonRepo := t.TempDir()
		branch := GetCurrentBranch(nonRepo)
		assert.Empty(t, branch)
	})

	t.Run("no heads prefix with ambiguous remote ref", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		// Create a branch and a remote-tracking ref that share
		// the same suffix. rev-parse --abbrev-ref and symbolic-ref
		// --short both add "heads/" to disambiguate.
		repo.Run("checkout", "-b", "user/feat")
		sha := repo.HeadSHA()
		repo.Run("update-ref", "refs/remotes/user/feat", sha)

		branch := GetCurrentBranch(repo.Dir)
		assert.Equal(t, "user/feat", branch,
			"should return clean branch name with ambiguous remote ref")
	})

	t.Run("no heads prefix with ambiguous tag", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		// A tag with the same name as the branch causes
		// symbolic-ref --short to return "heads/user/feat".
		repo.Run("checkout", "-b", "user/feat")
		repo.Run("tag", "user/feat")

		branch := GetCurrentBranch(repo.Dir)
		assert.Equal(t, "user/feat", branch,
			"should return clean branch name with ambiguous tag")
	})

	t.Run("no heads prefix in linked worktree with ambiguous refs", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		// Create a worktree on "user/feat/view", then add a remote
		// ref that shares the suffix. Both rev-parse --abbrev-ref
		// and symbolic-ref --short return "heads/user/feat/view"
		// in linked worktrees to disambiguate.
		wt := repo.AddWorktree("user/feat/view")
		sha := repo.HeadSHA()
		repo.Run(
			"update-ref",
			"refs/remotes/user/feat/view",
			sha,
		)

		branch := GetCurrentBranch(wt.Dir)
		assert.Equal(t, "user/feat/view", branch,
			"linked worktree should return clean branch name")
	})
}

func TestGetUpstream(t *testing.T) {
	t.Run("returns empty when no upstream configured", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		require.NoError(t, err)
		assert.Empty(t, upstream)
	})

	t.Run("returns upstream tracking branch", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")

		// Name the remote "upstream" to match the user's fork-style setup.
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")
		repo.Run("checkout", "-b", "feature", "--track", "upstream/main")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		require.NoError(t, err)
		assert.Equal(t, "upstream/main", upstream)
	})

	t.Run("returns upstream for named ref", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")

		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("checkout", "-b", "feature")

		// feature has no upstream, but main does.
		upstream, err := GetUpstream(repo.Dir, "main")
		require.NoError(t, err)
		assert.Equal(t, "origin/main", upstream)

		upstream, err = GetUpstream(repo.Dir, "feature")
		require.NoError(t, err)
		assert.Empty(t, upstream)
	})

	t.Run("empty ref defaults to HEAD", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")

		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")

		upstream, err := GetUpstream(repo.Dir, "")
		require.NoError(t, err)
		assert.Equal(t, "origin/main", upstream)
	})

	t.Run("accepts local-branch upstream", func(t *testing.T) {
		// `git branch -u <local-branch>` sets tracking against a local ref
		// under refs/heads/... (no remote involved). GetUpstream must not
		// reject this as "missing" just because refs/remotes/<upstream>
		// doesn't exist.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("checkout", "-b", "dev")
		repo.Run("branch", "-u", "main", "dev")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		require.NoError(t, err)
		assert.Equal(t, "main", upstream,
			"local-branch upstream should be returned, not dropped")
	})

	t.Run("errors when tracking ref is missing locally", func(t *testing.T) {
		// Tracking config set but refs/remotes/<upstream> does not resolve
		// (e.g., never fetched, or was manually removed). Callers must be
		// able to distinguish this from "no upstream configured" so the
		// user isn't silently switched to a different base branch that
		// could yield the wrong commit range.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")
		// Tracking config now points at refs/remotes/upstream/main. Remove it.
		repo.Run("update-ref", "-d", "refs/remotes/upstream/main")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		assert.Empty(t, upstream)
		var missing *UpstreamMissingError
		require.ErrorAs(t, err, &missing, "expected UpstreamMissingError, got %T: %v", err, err)
		assert.Equal(t, "upstream/main", missing.Upstream)
	})

	t.Run("errors when tracking is configured but never fetched", func(t *testing.T) {
		// Fresh repo with manual tracking config against a ref that has
		// never been fetched. rev-parse @{upstream} fails with exit 128,
		// but branch.<name>.remote/merge are set, so callers must still
		// see UpstreamMissingError, not ("", nil).
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("config", "branch.main.remote", "upstream")
		repo.Run("config", "branch.main.merge", "refs/heads/main")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		assert.Empty(t, upstream)
		var missing *UpstreamMissingError
		require.ErrorAs(t, err, &missing, "expected UpstreamMissingError, got %T: %v", err, err)
		assert.Equal(t, "upstream/main", missing.Upstream)
	})

	t.Run("errors for missing slash-named remote tracking config", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("config", "branch.main.remote", "company/fork")
		repo.Run("config", "branch.main.merge", "refs/heads/main")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		assert.Empty(t, upstream)
		var missing *UpstreamMissingError
		require.ErrorAs(t, err, &missing, "expected UpstreamMissingError, got %T: %v", err, err)
		assert.Equal(t, "company/fork/main", missing.Upstream)
	})

	t.Run("ignores url remote tracking config", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.remote", "https://example.com/fork.git")
		repo.Run("config", "branch.feature.merge", "refs/heads/feature")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		require.NoError(t, err)
		assert.Empty(t, upstream)
	})

	t.Run("handles branch names containing dots", func(t *testing.T) {
		// Regression: git parses section/subsection/key by splitting on the
		// first and last dots, so "branch.release/1.2.3.remote" correctly
		// extracts subsection "release/1.2.3" and key "remote". Confirm
		// GetUpstream returns UpstreamMissingError for a dotted branch name
		// whose tracking ref has been removed.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")
		repo.Run("checkout", "-b", "release/1.2.3", "--track", "upstream/main")
		repo.Run("update-ref", "-d", "refs/remotes/upstream/main")

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		assert.Empty(t, upstream)
		var missing *UpstreamMissingError
		require.ErrorAs(t, err, &missing,
			"dotted branch name must not silently fall back to default")
		assert.Equal(t, "upstream/main", missing.Upstream)
	})

	t.Run("errors when remote ref missing despite a colliding local ref", func(t *testing.T) {
		// Regression: an unqualified refExists check can pass for a local
		// branch whose short name equals the upstream short name, masking
		// a missing refs/remotes/<upstream>/... and causing downstream
		// merge-base to use the wrong ref. Verify the tracking config's
		// qualified ref explicitly.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")
		// Remove the remote-tracking ref that @{upstream} points to…
		repo.Run("update-ref", "-d", "refs/remotes/upstream/main")
		// …and create a local ref with the identical short name so an
		// unqualified rev-parse would succeed.
		head := repo.HeadSHA()
		repo.Run("update-ref", "refs/heads/upstream/main", head)

		upstream, err := GetUpstream(repo.Dir, "HEAD")
		assert.Empty(t, upstream)
		var missing *UpstreamMissingError
		require.ErrorAs(t, err, &missing,
			"lookalike local ref must not satisfy the remote upstream check")
		assert.Equal(t, "upstream/main", missing.Upstream)
	})
}

func TestHasUncommittedChanges(t *testing.T) {
	t.Run("no changes", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		hasChanges, err := HasUncommittedChanges(repo.Dir)
		require.NoError(t, err, "HasUncommittedChanges failed: %v", err)
		assert.False(t, hasChanges, "no changes should not report uncommitted changes")
	})

	t.Run("staged changes", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "modified")
		repo.Run("add", ".")

		hasChanges, err := HasUncommittedChanges(repo.Dir)
		require.NoError(t, err, "HasUncommittedChanges failed: %v", err)
		assert.True(t, hasChanges, "expected staged changes to be reported as dirty")
	})

	t.Run("unstaged changes", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "unstaged")

		hasChanges, err := HasUncommittedChanges(repo.Dir)
		require.NoError(t, err, "HasUncommittedChanges failed: %v", err)
		assert.True(t, hasChanges, "expected unstaged changes to be reported as dirty")
	})

	t.Run("untracked file", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("untracked.txt", "new")

		hasChanges, err := HasUncommittedChanges(repo.Dir)
		require.NoError(t, err, "HasUncommittedChanges failed: %v", err)
		assert.True(t, hasChanges, "expected untracked file to be reported as dirty")
	})
}

func TestGetDirtyDiff(t *testing.T) {
	t.Run("includes tracked file changes", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "modified\n")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err, "GetDirtyDiff failed: %v", err)
		assert.Contains(t, diff, "initial.txt", "expected dirty diff to include initial.txt")
		assert.Contains(t, diff, "+modified", "expected diff to contain +modified")
	})

	t.Run("includes untracked files", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("newfile.txt", "new content\n")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err, "GetDirtyDiff failed: %v", err)
		assert.Contains(t, diff, "newfile.txt", "expected dirty diff to include newfile.txt")
		assert.Contains(t, diff, "+new content", "expected diff to contain +new content")
		assert.Contains(t, diff, "new file mode", "expected diff to contain 'new file mode' header")
	})

	t.Run("includes both tracked and untracked", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "changed\n")
		repo.WriteFile("another.txt", "another\n")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err, "GetDirtyDiff failed: %v", err)
		assert.Contains(t, diff, "initial.txt", "expected dirty diff to include initial.txt")
		assert.Contains(t, diff, "another.txt", "expected dirty diff to include another.txt")
	})

	t.Run("handles binary files", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("binary.bin", "hello\x00world")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err, "GetDirtyDiff failed: %v", err)
		assert.Contains(t, diff, "binary.bin", "expected dirty diff to include binary.bin")
		assert.Contains(t, diff, "Binary file", "expected dirty diff to include binary file marker")
	})
}

func TestGetDirtyDiffNoCommits(t *testing.T) {
	repo := NewTestRepo(t)

	repo.WriteFile("newfile.txt", "content\n")
	repo.Run("add", ".")

	repo.WriteFile("untracked.txt", "untracked\n")

	diff, err := GetDirtyDiff(repo.Dir)
	require.NoError(t, err, "GetDirtyDiff failed on repo with no commits: %v", err)
	assert.Contains(t, diff, "newfile.txt", "expected diff to contain newfile.txt (staged)")
	assert.Contains(t, diff, "untracked.txt", "expected diff to contain untracked file marker")
}

func TestGetDirtyDiffStagedThenDeleted(t *testing.T) {
	repo := NewTestRepo(t)

	repo.WriteFile("staged.txt", "staged content\n")
	repo.Run("add", "staged.txt")

	err := os.Remove(filepath.Join(repo.Dir, "staged.txt"))
	require.NoError(t, err, "failed to remove staged.txt")

	diff, err := GetDirtyDiff(repo.Dir)
	require.NoError(t, err, "GetDirtyDiff failed: %v", err)
	assert.Contains(t, diff, "staged.txt", "expected diff to contain staged.txt (staged but deleted from working tree)")
	assert.Contains(t, diff, "staged content", "expected staged diff to include content")
}

func TestGetDirtyFilesChangedIncludesExcludedDependencyMetadata(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("package-lock.json", "lock\n")
	repo.WriteFile("go.sum", "sum\n")

	files, err := GetDirtyFilesChanged(repo.Dir)
	require.NoError(t, err)
	assert.Contains(t, files, "go.sum")
	assert.Contains(t, files, "package-lock.json")
}

func TestGetDirtyFilesChangedExpandsUntrackedDirectories(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("frontend/package-lock.json", "lock\n")
	repo.WriteFile("frontend/src/index.js", "console.log('x')\n")

	files, err := GetDirtyFilesChanged(repo.Dir)
	require.NoError(t, err)
	assert.Contains(t, files, "frontend/package-lock.json")
	assert.Contains(t, files, "frontend/src/index.js")
	assert.NotContains(t, files, "frontend/")
}

func TestFormatExcludeArgs(t *testing.T) {
	assert.Nil(t, FormatExcludeArgs(nil))
	assert.Nil(t, FormatExcludeArgs([]string{}))

	// Plain names get both file and directory forms
	assert.Equal(t,
		[]string{
			":(exclude,glob)**/foo.lock",
			":(exclude,glob)**/foo.lock/**",
			":(exclude,glob)**/*.min.js",
			":(exclude,glob)**/*.min.js/**",
		},
		FormatExcludeArgs([]string{"foo.lock", "*.min.js"}),
	)

	// Patterns with path separators get both exact and subtree forms
	assert.Equal(t,
		[]string{
			":(exclude,glob)vendor/dist",
			":(exclude,glob)vendor/dist/**",
		},
		FormatExcludeArgs([]string{"vendor/dist"}),
	)

	// Whitespace-only patterns are skipped
	assert.Equal(t,
		[]string{
			":(exclude,glob)**/keep",
			":(exclude,glob)**/keep/**",
		},
		FormatExcludeArgs([]string{" ", "keep", "  "}),
	)

	// Leading slash = root-anchored (no **/ prefix)
	assert.Equal(t,
		[]string{
			":(exclude,glob)vendor",
			":(exclude,glob)vendor/**",
		},
		FormatExcludeArgs([]string{"/vendor"}),
	)
}

func TestGetDiffExtraExcludes(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("keep.txt", "keep\n")
	repo.WriteFile("custom.lock", "lockdata\n")
	repo.CommitAll("add files")

	sha := repo.HeadSHA()

	diff, err := GetDiff(repo.Dir, sha, "custom.lock")
	require.NoError(t, err)
	assert.Contains(t, diff, "keep.txt")
	assert.NotContains(t, diff, "custom.lock")
}

func TestGetRangeDiffExcludesDependencyMetadataBodies(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("frontend/package.json", `{"dependencies":{"react":"18.2.0"}}`+"\n")
	repo.WriteFile("frontend/package-lock.json", `{"packages":{"":{"dependencies":{"react":"18.2.0"}}}}`+"\n")
	repo.WriteFile("go.mod", "module example.com/app\n\nrequire github.com/jackc/pgx/v5 v5.9.0\n")
	repo.WriteFile("go.sum", "github.com/jackc/pgx/v5 v5.9.0 h1:old\n")
	repo.CommitAll("add dependency metadata")

	repo.WriteFile("frontend/package.json", `{"dependencies":{"react":"18.3.1"}}`+"\n")
	repo.WriteFile("frontend/package-lock.json", `{"packages":{"":{"dependencies":{"react":"18.3.1"}}}}`+"\n")
	repo.WriteFile("go.mod", "module example.com/app\n\nrequire github.com/jackc/pgx/v5 v5.10.0\n")
	repo.WriteFile("go.sum", "github.com/jackc/pgx/v5 v5.10.0 h1:new\n")
	repo.CommitAll("bump dependency metadata")

	diff, err := GetRangeDiff(repo.Dir, "HEAD~1..HEAD")
	require.NoError(t, err)
	assert.Contains(t, diff, "frontend/package.json")
	assert.Contains(t, diff, "go.mod")
	assert.NotContains(t, diff, "frontend/package-lock.json")
	assert.NotContains(t, diff, "go.sum")
}

func TestGetDiffExcludesNestedFiles(t *testing.T) {
	// Verify that built-in and extra excludes work at any depth
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("keep.txt", "keep\n")
	repo.WriteFile("sub/.cache/generated.txt", "nested builtin\n")
	repo.WriteFile("sub/deep/custom.lock", "nested custom\n")
	repo.CommitAll("add nested files")

	sha := repo.HeadSHA()

	diff, err := GetDiff(repo.Dir, sha, "custom.lock")
	require.NoError(t, err)
	assert.Contains(t, diff, "keep.txt")
	assert.NotContains(t, diff, ".cache/generated.txt",
		"built-in exclude should match nested cache files")
	assert.NotContains(t, diff, "custom.lock",
		"extra exclude should match nested custom.lock")
}

func setupDiffExcludesGeneratedFilesTest(t *testing.T) (*TestRepo, string) {
	t.Helper()
	repo := NewTestRepoWithCommit(t)

	repo.WriteFile(".beads/notes.md", "beads\n")
	repo.WriteFile(".gocache/object", "cache\n")
	repo.WriteFile(".cache/object", "cache\n")
	repo.WriteFile(".kata.local.toml", "[server]\nurl = \"http://x\"\n")
	repo.WriteFile(".kata.toml", "[project]\nname = \"demo\"\n")
	repo.WriteFile("keep.txt", "keep\n")

	repo.CommitAll("add files")

	sha := repo.HeadSHA()
	return repo, sha
}

func TestGetDiffExcludesGeneratedFiles(t *testing.T) {
	assertExcluded := func(t *testing.T, diff string) {
		t.Helper()
		require.Contains(t, diff, "keep.txt", "expected generated files filter to retain keep.txt")
		require.Contains(t, diff, ".kata.toml", "expected committed kata binding to remain reviewable")
		require.Contains(t, diff, ".kata.local.toml",
			"a committed .kata.local.toml steers kata binding resolution and must stay reviewable")
		require.NotContains(t, diff, ".beads/", "expected generated files filter to exclude .beads files")
		require.NotContains(t, diff, ".gocache/", "expected generated files filter to exclude .gocache files")
		require.NotContains(t, diff, ".cache/", "expected generated files filter to exclude .cache files")
	}

	t.Run("GetDiff", func(t *testing.T) {
		repo, sha := setupDiffExcludesGeneratedFilesTest(t)
		diff, err := GetDiff(repo.Dir, sha)
		require.NoError(t, err, "GetDiff failed: %v", err)
		assertExcluded(t, diff)
	})

	t.Run("GetRangeDiff", func(t *testing.T) {
		repo, _ := setupDiffExcludesGeneratedFilesTest(t)
		diff, err := GetRangeDiff(repo.Dir, "HEAD~1..HEAD")
		require.NoError(t, err, "GetRangeDiff failed: %v", err)
		assertExcluded(t, diff)
	})
}

func TestGetDiffExcludesSlashedDirectory(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("keep.txt", "keep\n")
	repo.WriteFile("vendor/dist/bundle.js", "bundled\n")
	repo.WriteFile("vendor/dist/deep/util.js", "util\n")
	repo.CommitAll("add vendor/dist files")

	sha := repo.HeadSHA()

	t.Run("GetDiff", func(t *testing.T) {
		diff, err := GetDiff(repo.Dir, sha, "vendor/dist")
		require.NoError(t, err)
		assert.Contains(t, diff, "keep.txt")
		assert.NotContains(t, diff, "bundle.js",
			"slashed exclude should filter tracked dir contents")
		assert.NotContains(t, diff, "util.js",
			"slashed exclude should filter nested tracked files")
	})

	t.Run("GetRangeDiff", func(t *testing.T) {
		diff, err := GetRangeDiff(repo.Dir, "HEAD~1..HEAD", "vendor/dist")
		require.NoError(t, err)
		assert.Contains(t, diff, "keep.txt")
		assert.NotContains(t, diff, "bundle.js",
			"slashed exclude should filter tracked dir contents")
		assert.NotContains(t, diff, "util.js",
			"slashed exclude should filter nested tracked files")
	})
}

func TestGetDiffLimited(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("large.txt", strings.Repeat("line\n", 20000))
	repo.CommitAll("large change")

	diff, truncated, err := GetDiffLimited(repo.Dir, repo.HeadSHA(), 1024)
	require.NoError(t, err)
	assert.True(t, truncated, "expected limited diff read to report truncation")
	assert.LessOrEqual(t, len(diff), 1024)
}

func TestGetRangeDiffLimited(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("large.txt", strings.Repeat("line\n", 20000))
	repo.CommitAll("large change")

	diff, truncated, err := GetRangeDiffLimited(repo.Dir, "HEAD~1..HEAD", 1024)
	require.NoError(t, err)
	assert.True(t, truncated, "expected limited range diff read to report truncation")
	assert.LessOrEqual(t, len(diff), 1024)
}

func TestGetDiffLimitedTruncatesUTF8Safely(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("large.txt", strings.Repeat("世界\n", 20000))
	repo.CommitAll("large unicode change")

	fullDiff, err := GetDiff(repo.Dir, repo.HeadSHA())
	require.NoError(t, err)

	splitAt := -1
	for i := 0; i < len(fullDiff); i++ {
		if fullDiff[i] >= utf8.RuneSelf {
			splitAt = i + 1
			break
		}
	}
	require.Positive(t, splitAt, "expected diff to contain multibyte UTF-8 content")

	diff, truncated, err := GetDiffLimited(repo.Dir, repo.HeadSHA(), splitAt)
	require.NoError(t, err)
	assert.True(t, truncated, "expected limited diff read to report truncation")
	assert.LessOrEqual(t, len(diff), splitAt)
	assert.True(t, utf8.ValidString(diff), "limited diff output should remain valid UTF-8")
}

func TestGetDiffLimitedPreservesPrefixWithEarlierInvalidBytes(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	path := filepath.Join(repo.Dir, "legacy.txt")
	content := append([]byte("latin1: \xe9\n"), []byte(strings.Repeat("世界\n", 20000))...)
	err := os.WriteFile(path, content, 0o644)
	require.NoError(t, err)
	repo.Run("add", "legacy.txt")
	repo.Run("commit", "-m", "large legacy-encoded change")

	fullDiff, err := GetDiff(repo.Dir, repo.HeadSHA())
	require.NoError(t, err)

	diffBytes := []byte(fullDiff)
	invalidIdx := strings.Index(fullDiff, "\xe9")
	require.Positive(t, invalidIdx, "expected diff to contain earlier invalid UTF-8 bytes")

	splitAt := -1
	for i := invalidIdx + 1; i < len(diffBytes); i++ {
		if diffBytes[i] >= utf8.RuneSelf && utf8.RuneStart(diffBytes[i]) {
			splitAt = i + 1
			break
		}
	}
	require.Positive(t, splitAt, "expected diff to contain multibyte UTF-8 content after invalid bytes")

	diff, truncated, err := GetDiffLimited(repo.Dir, repo.HeadSHA(), splitAt)
	require.NoError(t, err)
	assert.True(t, truncated, "expected limited diff read to report truncation")
	assert.True(t, utf8.ValidString(diff), "limited diff output should remain valid UTF-8")
	assert.Contains(t, diff, "latin1:", "sanitized limited diff should preserve the earlier valid prefix")
	assert.NotEmpty(t, diff, "sanitized limited diff should not collapse to an empty string")
}

func TestSanitizeToValidUTF8PreservesEarlierBytesWhileRepairingTail(t *testing.T) {
	input := []byte("latin1: \xe9\nunicode: ")
	input = append(input, []byte("世界")...)
	input = input[:len(input)-1] // split the final multibyte rune

	output := sanitizeToValidUTF8(input)

	assert.True(t, utf8.ValidString(output), "sanitized output should be valid UTF-8")
	assert.Contains(t, output, "latin1:", "sanitized output should preserve the earlier valid prefix")
	assert.NotEmpty(t, output, "sanitized output should not collapse to an empty string")
}

func TestGetDirtyDiffExcludesUntrackedFiles(t *testing.T) {
	t.Run("plain directory exclude", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.WriteFile("new.go", "package main\n")
		repo.WriteFile("vendor/dep.go", "vendored\n")
		repo.WriteFile("vendor/sub/util.go", "util\n")

		diff, err := GetDirtyDiff(repo.Dir, "vendor")
		require.NoError(t, err)
		assert.Contains(t, diff, "new.go")
		assert.NotContains(t, diff, "vendor/dep.go")
		assert.NotContains(t, diff, "vendor/sub/util.go")
	})

	t.Run("dependency metadata bodies are excluded", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.WriteFile("keep.go", "package main\n")
		repo.WriteFile("sub/uv.lock", "lock\n")
		repo.WriteFile("package-lock.json", "lock\n")
		repo.WriteFile("deep/Cargo.lock", "lock\n")
		repo.WriteFile("sub/cargo.lock", "lock\n")
		repo.WriteFile("go.sum", "sum\n")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err)
		assert.Contains(t, diff, "keep.go")
		assert.NotContains(t, diff, "uv.lock")
		assert.NotContains(t, diff, "package-lock.json")
		assert.NotContains(t, diff, "Cargo.lock")
		assert.NotContains(t, diff, "cargo.lock")
		assert.NotContains(t, diff, "go.sum")
	})

	t.Run("basename glob pattern", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.WriteFile("keep.js", "ok\n")
		repo.WriteFile("app.min.js", "minified\n")
		repo.WriteFile("sub/lib.min.js", "nested\n")

		diff, err := GetDirtyDiff(repo.Dir, "*.min.js")
		require.NoError(t, err)
		assert.Contains(t, diff, "keep.js")
		assert.NotContains(t, diff, "app.min.js")
		assert.NotContains(t, diff, "lib.min.js")
	})

	t.Run("slashed rooted pattern", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.WriteFile("keep.txt", "ok\n")
		repo.WriteFile("vendor/dist/bundle.js", "bundled\n")
		repo.WriteFile("vendor/dist/deep/util.js", "util\n")

		diff, err := GetDirtyDiff(repo.Dir, "vendor/dist")
		require.NoError(t, err)
		assert.Contains(t, diff, "keep.txt")
		assert.NotContains(t, diff, "bundle.js")
		assert.NotContains(t, diff, "util.js")
	})
}

func TestIsWorkingTreeClean(t *testing.T) {
	t.Run("clean tree returns true", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		require.True(t, IsWorkingTreeClean(repo.Dir), "expected clean tree for clean tree case")
	})

	t.Run("dirty tree with modified file returns false", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "modified")

		require.False(t, IsWorkingTreeClean(repo.Dir), "expected modified file to make tree dirty")
	})

	t.Run("dirty tree with untracked file returns false", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("untracked.txt", "untracked")

		require.False(t, IsWorkingTreeClean(repo.Dir), "expected untracked file to make tree dirty")
	})
}

func TestResetWorkingTree(t *testing.T) {
	t.Run("resets modified files", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "modified")
		assert.False(t, IsWorkingTreeClean(repo.Dir), "expected tree to be dirty before reset")

		err := ResetWorkingTree(repo.Dir)
		require.NoError(t, err, "ResetWorkingTree failed: %v", err)
		assert.True(t, IsWorkingTreeClean(repo.Dir), "expected tree to be clean after reset")

		content, err := os.ReadFile(filepath.Join(repo.Dir, "initial.txt"))
		require.NoError(t, err)
		assert.Equal(t, "initial content", string(content), "expected file content 'initial content', got %q", string(content))
	})

	t.Run("removes untracked files", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		untrackedFile := filepath.Join(repo.Dir, "untracked.txt")
		repo.WriteFile("untracked.txt", "untracked")
		assert.False(t, IsWorkingTreeClean(repo.Dir), "expected tree to be dirty before reset")

		err := ResetWorkingTree(repo.Dir)
		require.NoError(t, err, "ResetWorkingTree failed: %v", err)

		require.True(t, IsWorkingTreeClean(repo.Dir), "expected tree to be clean after reset")

		_, err = os.Stat(untrackedFile)
		require.Error(t, err, "expected untracked file to be removed after reset")
	})

	t.Run("resets staged changes", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		repo.WriteFile("initial.txt", "staged changes")
		repo.Run("add", ".")
		assert.False(t, IsWorkingTreeClean(repo.Dir), "expected tree to be dirty before reset")

		err := ResetWorkingTree(repo.Dir)
		require.NoError(t, err, "ResetWorkingTree failed: %v", err)
		assert.True(t, IsWorkingTreeClean(repo.Dir), "expected tree to be clean after reset")

		content, err := os.ReadFile(filepath.Join(repo.Dir, "initial.txt"))
		require.NoError(t, err)
		assert.Equal(t, "initial content", string(content), "expected file content 'initial content', got %q", string(content))
	})
}

func TestUpstreamIsTrunk(t *testing.T) {
	t.Run("trunk-named upstream matches", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")

		assert.True(t, UpstreamIsTrunk(repo.Dir, "HEAD"))
	})

	t.Run("self-counterpart upstream does not match", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")
		repo.Run("checkout", "-b", "feature")
		repo.Run("push", "-u", "origin", "feature")

		// feature tracks origin/feature (its own remote counterpart) — not trunk.
		assert.False(t, UpstreamIsTrunk(repo.Dir, "HEAD"))
	})

	t.Run("multi-remote trunk matches", func(t *testing.T) {
		// Fork workflow: local main tracks upstream/main while the default
		// branch is origin/main. Both refs strip to "main" → trunk.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")
		upRemote := NewBareTestRepo(t)
		repo.Run("remote", "add", "upstream", upRemote.Dir)
		repo.Run("push", "upstream", "main")
		repo.Run("branch", "-u", "upstream/main", "main")

		assert.True(t, UpstreamIsTrunk(repo.Dir, "HEAD"))
	})

	t.Run("returns false when no upstream is configured", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		assert.False(t, UpstreamIsTrunk(repo.Dir, "HEAD"))
	})

	t.Run("returns false when default branch cannot be detected", func(t *testing.T) {
		// Branch tracks some upstream, but origin/HEAD and main/master are
		// all missing so GetDefaultBranch fails.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/trunk")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "trunk")

		assert.False(t, UpstreamIsTrunk(repo.Dir, "HEAD"))
	})

	t.Run("feature branch whose leaf matches default is not trunk", func(t *testing.T) {
		// Regression: a feature branch tracking e.g. origin/team/main has
		// last path segment "main" and would wrongly match the default
		// leaf. UpstreamIsTrunk must compare the full branch name after
		// stripping the configured remote prefix, not just the leaf.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")
		// Simulate a feature branch whose remote-tracking ref ends in "main"
		// but isn't trunk.
		head := repo.HeadSHA()
		repo.Run("update-ref", "refs/remotes/origin/team/main", head)
		repo.Run("checkout", "-b", "team/main")
		repo.Run("config", "branch.team/main.remote", "origin")
		repo.Run("config", "branch.team/main.merge", "refs/heads/team/main")

		assert.False(t, UpstreamIsTrunk(repo.Dir, "HEAD"),
			"branch tracking origin/team/main is not trunk — its branch part is team/main, not main")
	})

	t.Run("accepts refs/heads/-qualified branch refs", func(t *testing.T) {
		// Regression: callers that pass a fully-qualified ref (e.g.,
		// "refs/heads/feature" or output of ResolveSHA) must still hit
		// the branch.<name>.* config keys after the prefix is stripped.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")

		assert.True(t, UpstreamIsTrunk(repo.Dir, "refs/heads/main"),
			"refs/heads/-qualified ref should resolve to the same branch config as 'HEAD'")
	})

	t.Run("local-branch upstream is not trunk even if its name matches default", func(t *testing.T) {
		// Regression: a branch can track a local branch via
		// branch.<name>.remote = ".". If the tracked local branch is
		// literally named "origin/main" in a repo whose default is
		// origin/main, stripRemotePrefix would normalize both to "main"
		// and misclassify the local upstream as trunk. The namespace
		// check (refs/heads/... vs refs/remotes/...) must reject the
		// local-branch upstream.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")
		head := repo.HeadSHA()
		// Local branch literally named "origin/main".
		repo.Run("update-ref", "refs/heads/origin/main", head)
		// New branch whose upstream is the LOCAL "origin/main", via remote=".".
		repo.Run("checkout", "-b", "pinned", head)
		repo.Run("config", "branch.pinned.remote", ".")
		repo.Run("config", "branch.pinned.merge", "refs/heads/origin/main")

		assert.False(t, UpstreamIsTrunk(repo.Dir, "HEAD"),
			"local-branch upstream named origin/main must not be classified as trunk")
	})
}

func TestIsOnBaseBranch(t *testing.T) {
	t.Run("matches bare local name", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		assert.True(t, IsOnBaseBranch(repo.Dir, "main", "main"))
		assert.False(t, IsOnBaseBranch(repo.Dir, "feature", "main"))
	})

	t.Run("matches origin-prefixed ref", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")

		assert.True(t, IsOnBaseBranch(repo.Dir, "main", "origin/main"))
		assert.False(t, IsOnBaseBranch(repo.Dir, "feature", "origin/main"))
	})

	t.Run("matches non-origin remote prefix", func(t *testing.T) {
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")

		assert.True(t, IsOnBaseBranch(repo.Dir, "main", "upstream/main"))
		assert.False(t, IsOnBaseBranch(repo.Dir, "feature", "upstream/main"))
	})

	t.Run("does not strip slash when no matching remote-tracking ref", func(t *testing.T) {
		// feature/foo is a local branch, not origin/main style. Even when a
		// remote named "feature" is configured, we must not treat base
		// "feature/foo" as if it were a remote-tracking ref and strip the
		// prefix — that would falsely match a local branch named "foo".
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "feature", remote.Dir)
		repo.Run("checkout", "-b", "feature/foo")
		repo.CommitFile("b.txt", "b", "work")
		repo.Run("checkout", "-b", "foo", "main")

		// Current branch "foo" vs base "feature/foo" — refs/remotes/feature/foo
		// does not exist, so the prefix must not be stripped.
		assert.False(t, IsOnBaseBranch(repo.Dir, "foo", "feature/foo"))
		// And the real "on-base" case for a local branch with a slash still works.
		assert.True(t, IsOnBaseBranch(repo.Dir, "feature/foo", "feature/foo"))
	})

	t.Run("empty current branch does not match", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		assert.False(t, IsOnBaseBranch(repo.Dir, "", "main"))
	})

	t.Run("multi-slash remote name strips full prefix", func(t *testing.T) {
		// A remote named "company/fork" produces tracking refs under
		// refs/remotes/company/fork/<branch>. Stripping only the first
		// slash ("company/") would leave "fork/main" and wrongly match
		// a local branch of that name. The full remote prefix
		// "company/fork/" must be stripped to yield "main".
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "company/fork", remote.Dir)
		repo.Run("push", "-u", "company/fork", "main")

		assert.True(t, IsOnBaseBranch(repo.Dir, "main", "company/fork/main"),
			"current=main on base=company/fork/main must strip full remote prefix")
		assert.False(t, IsOnBaseBranch(repo.Dir, "fork/main", "company/fork/main"),
			"current=fork/main must not falsely match after a single-slash strip")
	})

	t.Run("pathological local branch named like remote tracking does not equality-match", func(t *testing.T) {
		// Regression: the raw equality fast-path would treat a local branch
		// named "origin/main" as "already on base" when base was the
		// remote-tracking ref of the same short name. The two are distinct
		// refs in different namespaces; when both exist, the guardrail
		// must refuse to match rather than assume they're the same.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		head := repo.HeadSHA()
		// Create a local branch literally named "origin/main" so both
		// refs/heads/origin/main AND refs/remotes/origin/main resolve.
		repo.Run("update-ref", "refs/heads/origin/main", head)

		assert.False(t, IsOnBaseBranch(repo.Dir, "origin/main", "origin/main"),
			"ambiguous name in both refs/heads and refs/remotes must not match")
	})

	t.Run("origin-prefixed local branch without remote ref is not base", func(t *testing.T) {
		// Regression: the legacy LocalBranchName shortcut stripped "origin/"
		// unconditionally, so a local branch literally named "origin/foo"
		// (no refs/remotes/origin/foo) made currentBranch "foo" appear
		// "already on base origin/foo". That would wrongly block
		// review --branch / refine on a perfectly valid feature branch.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		// Create a local branch literally named "origin/foo" — no remote involved.
		repo.Run("branch", "origin/foo")

		assert.False(t, IsOnBaseBranch(repo.Dir, "foo", "origin/foo"),
			"local branch origin/foo without a remote-tracking ref must not match 'foo'")
		assert.True(t, IsOnBaseBranch(repo.Dir, "origin/foo", "origin/foo"),
			"exact-name match still works")
	})

	t.Run("ambiguous slash-containing base refuses to match", func(t *testing.T) {
		// Pathological case: both refs/heads/feature/foo and
		// refs/remotes/feature/foo exist. The caller's intent is unclear
		// (local branch or remote-tracking?), so the safe response is to
		// refuse to match either way. The downstream merge-base / range
		// check will surface any real "nothing to review" condition.
		remote := NewBareTestRepo(t)
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "feature", remote.Dir)
		repo.Run("branch", "feature/foo")
		head := repo.HeadSHA()
		repo.Run("update-ref", "refs/remotes/feature/foo", head)

		assert.False(t, IsOnBaseBranch(repo.Dir, "foo", "feature/foo"),
			"ambiguous ref must not be stripped")
		assert.False(t, IsOnBaseBranch(repo.Dir, "feature/foo", "feature/foo"),
			"ambiguous ref must not equality-match either")
	})
}

func TestLocalBranchName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"main", "main"},
		{"origin/main", "main"},
		{"origin/master", "master"},
		{"feature/foo", "feature/foo"},
		{"origin/feature/foo", "feature/foo"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := LocalBranchName(tt.input)
			assert.Equal(t, tt.want, got, "LocalBranchName(%q) = %q, want %q", tt.input, got, tt.want)
		})
	}
}

func setupRangeFilesChangedTest(t *testing.T) (*TestRepo, string) {
	t.Helper()
	repo := NewTestRepo(t)
	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("base.txt", "base", "base commit")
	baseSHA := repo.HeadSHA()

	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("new.go", "package main", "add go file")
	repo.CommitFile("docs.md", "# Docs", "add docs")
	repo.CommitFile("config.yml", "key: val", "add config")

	return repo, baseSHA
}

func TestGetRangeFilesChanged(t *testing.T) {
	t.Run("returns changed files in range", func(t *testing.T) {
		repo, baseSHA := setupRangeFilesChangedTest(t)
		files, err := GetRangeFilesChanged(repo.Dir, baseSHA+"..HEAD")
		require.NoError(t, err, "GetRangeFilesChanged failed: %v", err)
		require.NoError(t, err, "expected 3 files, got %d: %v", len(files), files)

		found := map[string]bool{}
		for _, f := range files {
			found[f] = true
		}
		for _, want := range []string{"new.go", "docs.md", "config.yml"} {
			assert.True(t, found[want], "expected %s in changed files, got %v", want, files)
		}
	})

	t.Run("empty range returns nil", func(t *testing.T) {
		repo, _ := setupRangeFilesChangedTest(t)
		files, err := GetRangeFilesChanged(repo.Dir, "HEAD..HEAD")
		require.NoError(t, err, "GetRangeFilesChanged failed: %v", err)
		assert.Empty(t, files, "expected 0 files for empty range, got %d: %v", len(files), files)
	})
}

func TestCreateCommitPreCommitHookOutput(t *testing.T) {
	repo := NewTestRepoWithCommit(t)

	repo.InstallHook("pre-commit",
		"#!/bin/sh\necho 'error: trailing whitespace on line 42' >&2\nexit 1\n")

	repo.WriteFile("new.txt", "content")
	repo.Run("add", "new.txt")

	_, err := CreateCommit(repo.Dir, "should fail")
	require.Error(t, err, "expected CreateCommit to fail with pre-commit hook")

	assert.
		Contains(t, err.Error(), "trailing whitespace on line 42", "expected error to contain hook output, got: %v", err)

	var commitErr *CommitError
	require.ErrorAs(t, err, &commitErr, "expected CommitError type")
	assert.True(t, commitErr.HookFailed, "expected HookFailed=true for pre-commit hook rejection")
}

func TestCreateCommitExecutableHookReceivesRepoPWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("direct binary hooks without .exe are not portable on Windows")
	}

	repo := NewTestRepoWithCommit(t)
	t.Setenv("PWD", t.TempDir())
	t.Setenv("ROBOREV_GIT_HOOK_PWD_HELPER", "1")
	installSelfAsHook(t, repo.Dir, "pre-commit")

	repo.WriteFile("pwd.txt", "content")

	sha, err := CreateCommit(repo.Dir, "commit with pwd hook")
	require.NoError(t, err)
	assert.Equal(t, sha, repo.HeadSHA())
}

func installSelfAsHook(t *testing.T, repoDir, name string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(exe)
	require.NoError(t, err)

	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	err = os.MkdirAll(hooksDir, 0o755)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(hooksDir, name), data, 0o755)
	require.NoError(t, err)
}

func TestCreateCommitHookFailedProbePreservesEnvOnlyIdentity(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.Run("config", "--unset", "user.name")
	repo.Run("config", "--unset", "user.email")

	realGit, err := exec.LookPath("git")
	require.NoError(t, err, "locate real git")

	wrapperDir := t.TempDir()
	writeGitDryRunIdentityProbeWrapper(t, wrapperDir)
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GIT_AUTHOR_NAME", "Env Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "env-author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Env Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "env-committer@example.com")

	repo.InstallHook("pre-commit",
		"#!/bin/sh\necho 'blocked by env-only identity hook' >&2\nexit 1\n")

	repo.WriteFile("new.txt", "content")

	_, err = CreateCommit(repo.Dir, "should fail")
	require.Error(t, err, "expected CreateCommit to fail with pre-commit hook")

	var commitErr *CommitError
	require.ErrorAs(t, err, &commitErr, "expected CommitError type")
	assert.True(t, commitErr.HookFailed, "expected HookFailed=true with env-only identity")
}

func writeGitDryRunIdentityProbeWrapper(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "git.bat")
		script := `@echo off
setlocal EnableExtensions
set is_commit=0
set is_dry_run=0
for %%A in (%*) do (
  if "%%~A"=="commit" set is_commit=1
  if "%%~A"=="--dry-run" set is_dry_run=1
)
if "%is_commit%%is_dry_run%"=="11" (
  if not "%GIT_AUTHOR_NAME%"=="" exit /b 0
  echo missing env identity in dry-run 1>&2
  exit /b 1
)
"%REAL_GIT%" %*
`
		err := os.WriteFile(path, []byte(script), 0o755)
		require.NoError(t, err)
		return
	}

	path := filepath.Join(dir, "git")
	script := `#!/bin/sh
is_commit=0
is_dry_run=0
for arg do
	if [ "$arg" = "commit" ]; then
		is_commit=1
	fi
	if [ "$arg" = "--dry-run" ]; then
		is_dry_run=1
	fi
done
if [ "$is_commit$is_dry_run" = "11" ]; then
	if [ -n "$GIT_AUTHOR_NAME" ]; then
		exit 0
	fi
	echo "missing env identity in dry-run" >&2
	exit 1
fi
exec "$REAL_GIT" "$@"
`
	err := os.WriteFile(path, []byte(script), 0o755)
	require.NoError(t, err)
}

func TestCommitErrorHookFailedFalseWhenNothingToCommit(t *testing.T) {
	repo := NewTestRepoWithCommit(t)

	repo.InstallHook("pre-commit", "#!/bin/sh\nexit 0\n")

	_, err := CreateCommit(repo.Dir, "empty commit")
	require.Error(t, err, "expected CreateCommit to fail")

	var commitErr *CommitError
	require.ErrorAs(t, err, &commitErr, "expected CommitError type")
	assert.False(t, commitErr.HookFailed, "expected HookFailed=false for dry-run")
}

func TestCommitErrorHookFailedCommitMsgHook(t *testing.T) {
	repo := NewTestRepoWithCommit(t)

	repo.InstallHook("commit-msg",
		"#!/bin/sh\necho 'bad commit message format' >&2\nexit 1\n")

	repo.WriteFile("new.txt", "content")
	repo.Run("add", "new.txt")

	_, err := CreateCommit(repo.Dir, "should fail")
	require.Error(t, err, "expected CreateCommit to fail with commit-msg hook")

	var commitErr *CommitError
	require.ErrorAs(t, err, &commitErr, "expected CommitError type")
	assert.True(t, commitErr.HookFailed, "expected HookFailed=true for commit-msg hook rejection")
}

func TestCommitErrorHookFailedFalseForGPGSigningFailure(t *testing.T) {
	repo := NewTestRepoWithCommit(t)

	repo.Run("config", "commit.gpgsign", "true")

	dummyGPG := filepath.Join(repo.Dir, "fail-gpg")
	if runtime.GOOS == "windows" {
		dummyGPG += ".bat"
		repo.WriteFile("fail-gpg.bat", "@echo off\nexit /b 1\n")
	} else {
		repo.WriteFile("fail-gpg", "#!/bin/sh\nexit 1\n")
		err := os.Chmod(dummyGPG, 0o755)
		require.NoError(t, err, "failed to chmod fail-gpg")
	}

	repo.Run("config", "gpg.program", dummyGPG)
	repo.Run("config", "user.signingkey", "DEADBEEF00000000")

	repo.WriteFile("new.txt", "content")
	repo.Run("add", "new.txt")

	_, err := CreateCommit(repo.Dir, "should fail from gpg")
	require.Error(t, err, "expected commit to fail due to gpg.program=false")

	var commitErr *CommitError
	require.ErrorAs(t, err, &commitErr, "expected CommitError type, got: %T", err)
	assert.False(t, commitErr.HookFailed, "HookFailed should be false for GPG signing failure (no hooks installed)")
}

func TestCreateCommitPreservesGitIdentityEnvWhileIgnoringRepoEnv(t *testing.T) {
	target := NewTestRepoWithCommit(t)
	leaked := NewTestRepoWithCommit(t)
	leakedHead := leaked.HeadSHA()

	t.Setenv("GIT_DIR", filepath.Join(leaked.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", leaked.Dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(leaked.Dir, ".git", "index"))
	t.Setenv("GIT_IMPLICIT_WORK_TREE", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Env Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "env-author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Env Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "env-committer@example.com")

	target.WriteFile("created.txt", "content")
	sha, err := CreateCommit(target.Dir, "commit with env identity")
	require.NoError(t, err)

	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	os.Unsetenv("GIT_INDEX_FILE")
	os.Unsetenv("GIT_IMPLICIT_WORK_TREE")

	assert.Equal(t, sha, target.HeadSHA())
	assert.Equal(t, leakedHead, leaked.HeadSHA(), "leaked repo env must not redirect the commit")
	assert.Equal(t, "Env Author <env-author@example.com>", target.Run("show", "-s", "--format=%an <%ae>", sha))
	assert.Equal(t, "Env Committer <env-committer@example.com>", target.Run("show", "-s", "--format=%cn <%ce>", sha))
}

func TestCreateCommitPreservesGitConfigEnvWhileIgnoringRepoEnv(t *testing.T) {
	target := NewTestRepoWithCommit(t)
	leaked := NewTestRepoWithCommit(t)
	leakedHead := leaked.HeadSHA()

	t.Setenv("GIT_DIR", filepath.Join(leaked.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", leaked.Dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(leaked.Dir, ".git", "index"))
	t.Setenv("GIT_IMPLICIT_WORK_TREE", "0")
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", leaked.Dir)
	t.Setenv("GIT_CONFIG_KEY_1", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_1", "Config Author")
	t.Setenv("GIT_CONFIG_KEY_2", "user.email")
	t.Setenv("GIT_CONFIG_VALUE_2", "config-author@example.com")

	target.WriteFile("created.txt", "content")
	sha, err := CreateCommit(target.Dir, "commit with env config")
	require.NoError(t, err)

	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	os.Unsetenv("GIT_INDEX_FILE")
	os.Unsetenv("GIT_IMPLICIT_WORK_TREE")

	assert.Equal(t, sha, target.HeadSHA())
	assert.Equal(t, leakedHead, leaked.HeadSHA(), "leaked repo env must not redirect the commit")
	assert.Equal(t, "Config Author <config-author@example.com>", target.Run("show", "-s", "--format=%an <%ae>", sha))
}

func TestCreateCommitPreservesGitConfigParametersIdentity(t *testing.T) {
	target := NewTestRepoWithCommit(t)
	leaked := NewTestRepoWithCommit(t)
	leakedHead := leaked.HeadSHA()

	t.Setenv("GIT_DIR", filepath.Join(leaked.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", leaked.Dir)
	t.Setenv("GIT_IMPLICIT_WORK_TREE", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS",
		"'core.worktree'='"+leaked.Dir+"' 'core.fileMode'= 'color.ui' 'user.name'='Param O'\\''Connor' 'user.email'='param@example.com'")

	target.WriteFile("created.txt", "content")
	sha, err := CreateCommit(target.Dir, "commit with parameters identity")
	require.NoError(t, err)

	os.Unsetenv("GIT_DIR")
	os.Unsetenv("GIT_WORK_TREE")
	os.Unsetenv("GIT_IMPLICIT_WORK_TREE")

	assert.Equal(t, sha, target.HeadSHA())
	assert.Equal(t, leakedHead, leaked.HeadSHA(), "leaked repo env and config parameters must not redirect the commit")
	assert.Equal(t, "Param O'Connor <param@example.com>", target.Run("show", "-s", "--format=%an <%ae>", sha))
}

func TestCreateCommitPreservesGlobalConfigEnv(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.Run("config", "--unset", "user.name")
	repo.Run("config", "--unset", "user.email")

	globalCfg := filepath.Join(t.TempDir(), "global.gitconfig")
	err := os.WriteFile(globalCfg, []byte("[user]\n\tname = Global Author\n\temail = global@example.com\n"), 0o644)
	require.NoError(t, err)
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	repo.WriteFile("created.txt", "content")
	sha, err := CreateCommit(repo.Dir, "commit with global identity")
	require.NoError(t, err)

	assert.Equal(t, "Global Author <global@example.com>", repo.Run("show", "-s", "--format=%an <%ae>", sha))
}

func TestFilterGitCommitEnvStripsLocalEnvAndPreservesCommitIdentity(t *testing.T) {
	assert := assert.New(t)
	got := filterGitCommitEnv([]string{
		"PATH=/usr/bin",
		"GIT_DIR=/leaked/.git",
		"GIT_WORK_TREE=/leaked",
		"GIT_INDEX_FILE=/leaked/.git/index",
		"GIT_IMPLICIT_WORK_TREE=0",
		"GIT_GRAFT_FILE=/leaked/info/grafts",
		"GIT_REPLACE_REF_BASE=refs/replace/leaked",
		"GIT_INTERNAL_SUPER_PREFIX=leaked/",
		"GIT_SHALLOW_FILE=/leaked/shallow",
		"GIT_CONFIG_PARAMETERS='core.worktree'='/leaked' 'core.fileMode'= 'color.ui' 'committer.name'='Param Committer' 'committer.email'='param-committer@example.com'",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=core.worktree",
		"GIT_CONFIG_VALUE_0=/leaked",
		"GIT_CONFIG_KEY_1=user.name",
		"GIT_CONFIG_VALUE_1=Config Author",
		"GIT_CONFIG_KEY_2=user.email",
		"GIT_CONFIG_VALUE_2=config-author@example.com",
		"GIT_CONFIG_GLOBAL=/tmp/global.gitconfig",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_SSH_COMMAND=ssh -i /tmp/key",
		"GIT_AUTHOR_NAME=Env Author",
		"GIT_AUTHOR_EMAIL=env-author@example.com",
		"GIT_COMMITTER_NAME=Env Committer",
		"GIT_COMMITTER_EMAIL=env-committer@example.com",
	})
	joined := strings.Join(got, "\n")

	assert.NotContains(joined, "GIT_DIR=")
	assert.NotContains(joined, "GIT_WORK_TREE=")
	assert.NotContains(joined, "GIT_INDEX_FILE=")
	assert.NotContains(joined, "GIT_IMPLICIT_WORK_TREE=")
	assert.NotContains(joined, "GIT_GRAFT_FILE=")
	assert.NotContains(joined, "GIT_REPLACE_REF_BASE=")
	assert.NotContains(joined, "GIT_INTERNAL_SUPER_PREFIX=")
	assert.NotContains(joined, "GIT_SHALLOW_FILE=")
	assert.NotContains(joined, "GIT_CONFIG_PARAMETERS=")
	assert.NotContains(joined, "core.worktree")
	assert.Contains(joined, "GIT_AUTHOR_NAME=Env Author")
	assert.Contains(joined, "GIT_AUTHOR_EMAIL=env-author@example.com")
	assert.Contains(joined, "GIT_COMMITTER_NAME=Env Committer")
	assert.Contains(joined, "GIT_COMMITTER_EMAIL=env-committer@example.com")
	assert.Contains(joined, "GIT_CONFIG_KEY_0=user.name")
	assert.Contains(joined, "GIT_CONFIG_VALUE_0=Config Author")
	assert.Contains(joined, "GIT_CONFIG_KEY_1=user.email")
	assert.Contains(joined, "GIT_CONFIG_VALUE_1=config-author@example.com")
	assert.Contains(joined, "GIT_CONFIG_KEY_2=committer.name")
	assert.Contains(joined, "GIT_CONFIG_VALUE_2=Param Committer")
	assert.Contains(joined, "GIT_CONFIG_KEY_3=committer.email")
	assert.Contains(joined, "GIT_CONFIG_VALUE_3=param-committer@example.com")
	assert.Contains(joined, "GIT_CONFIG_COUNT=4")
	assert.Contains(joined, "GIT_CONFIG_GLOBAL=/tmp/global.gitconfig")
	assert.Contains(joined, "GIT_CONFIG_NOSYSTEM=1")
	assert.Contains(joined, "GIT_SSH_COMMAND=ssh -i /tmp/key")
}

func TestHasCommitHooksDetectsInstalledHooks(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	require.False(t, hasCommitHooks(repo.Dir), "expected pre-commit hook to be detected after install")

	repo.InstallHook("pre-commit", "#!/bin/sh\nexit 0\n")
	assert.True(t, hasCommitHooks(repo.Dir), "expected hasCommitHooks=true after installing pre-commit")
}

func TestHasCommitHooksIgnoresDirectories(t *testing.T) {
	repo := NewTestRepoWithCommit(t)

	hooksDir, err := GetHooksPath(repo.Dir)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(hooksDir, "pre-commit"), 0o755)
	require.NoError(t, err, "failed to create pre-commit directory")
	require.False(t, hasCommitHooks(repo.Dir), "expected directory named pre-commit not to be treated as hook file")
}

func setupAncestorTest(t *testing.T) (*TestRepo, string, string, string) {
	t.Helper()
	repo := NewTestRepo(t)
	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")

	repo.CommitFile("base.txt", "base", "base commit")
	baseSHA := repo.HeadSHA()

	repo.CommitFile("second.txt", "second", "second commit")
	secondSHA := repo.HeadSHA()

	repo.Run("checkout", baseSHA)
	repo.Run("checkout", "-b", "divergent")
	repo.CommitFile("divergent.txt", "divergent", "divergent commit")
	divergentSHA := repo.HeadSHA()

	return repo, baseSHA, secondSHA, divergentSHA
}

func TestIsAncestor(t *testing.T) {
	t.Run("base is ancestor of second", func(t *testing.T) {
		repo, baseSHA, secondSHA, _ := setupAncestorTest(t)
		isAnc, err := IsAncestor(repo.Dir, baseSHA, secondSHA)
		require.NoError(t, err, "unexpected error: %v", err)
		require.True(t, isAnc, "base is ancestor of divergent")
	})

	t.Run("second is not ancestor of base", func(t *testing.T) {
		repo, baseSHA, secondSHA, _ := setupAncestorTest(t)
		isAnc, err := IsAncestor(repo.Dir, secondSHA, baseSHA)
		require.NoError(t, err, "unexpected error: %v", err)
		assert.False(t, isAnc, "second should not be ancestor of base")
	})

	t.Run("divergent is not ancestor of second", func(t *testing.T) {
		repo, _, secondSHA, divergentSHA := setupAncestorTest(t)
		isAnc, err := IsAncestor(repo.Dir, divergentSHA, secondSHA)
		require.NoError(t, err, "unexpected error: %v", err)
		assert.False(t, isAnc, "expected divergent to NOT be ancestor of second (different branches)")
	})

	t.Run("base is ancestor of divergent", func(t *testing.T) {
		repo, baseSHA, _, divergentSHA := setupAncestorTest(t)
		isAnc, err := IsAncestor(repo.Dir, baseSHA, divergentSHA)
		require.NoError(t, err, "unexpected error: %v", err)
		require.True(t, isAnc, "commit should be ancestor of itself")
	})

	t.Run("commit is ancestor of itself", func(t *testing.T) {
		repo, baseSHA, _, _ := setupAncestorTest(t)
		isAnc, err := IsAncestor(repo.Dir, baseSHA, baseSHA)
		require.NoError(t, err, "unexpected error: %v", err)
		require.True(t, isAnc, "commit should be ancestor of itself")
	})

	t.Run("bad object returns error", func(t *testing.T) {
		repo, _, _, _ := setupAncestorTest(t)
		_, err := IsAncestor(repo.Dir, "badbadbadbadbadbadbadbadbadbadbadbadbad", "HEAD")
		require.Error(t, err, "bad object should return error")
	})
}

func TestRefMatchesBranchLineage(t *testing.T) {
	t.Run("feature branch excludes trunk refs and includes feature refs", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("trunk.txt", "trunk", "trunk commit")
		trunkSHA := repo.HeadSHA()
		repo.Run("checkout", "-b", "feature/lineage")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		featureSHA := repo.HeadSHA()

		assert.False(t, RefMatchesBranchLineage(repo.Dir, "feature/lineage", "HEAD", trunkSHA))
		assert.True(t, RefMatchesBranchLineage(repo.Dir, "feature/lineage", "HEAD", featureSHA))
		assert.True(t, RefMatchesBranchLineage(repo.Dir, "main", "main", trunkSHA))
	})

	t.Run("resolves symbolic and abbreviated refs before cached lookup", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("trunk.txt", "trunk", "trunk commit")
		repo.Run("checkout", "-b", "feature/lineage")
		repo.CommitFile("feature-1.txt", "feature", "feature commit 1")
		previousFeatureSHA := repo.HeadSHA()
		repo.CommitFile("feature-2.txt", "feature", "feature commit 2")
		featureSHA := repo.HeadSHA()

		matcher, err := NewBranchLineageMatcher(repo.Dir, "feature/lineage", "HEAD")
		require.NoError(t, err)
		assert.True(t, matcher.Matches("HEAD"))
		assert.True(t, matcher.Matches("HEAD~1"))
		assert.True(t, matcher.Matches("feature/lineage"))
		assert.True(t, matcher.Matches(featureSHA[:12]))
		assert.True(t, matcher.Matches(previousFeatureSHA[:12]+"..HEAD"))
	})

	t.Run("missing default branch fails closed", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/trunk")
		repo.CommitFile("trunk.txt", "trunk", "trunk commit")
		repo.Run("checkout", "-b", "feature/no-default")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		featureSHA := repo.HeadSHA()

		assert.False(t, RefMatchesBranchLineage(repo.Dir, "feature/no-default", "HEAD", featureSHA))
	})
}

func TestGetPatchID(t *testing.T) {
	t.Run("stable across rebase", func(t *testing.T) {
		repo := NewTestRepo(t)

		repo.Run("checkout", "-b", "main")
		repo.CommitFile("base.txt", "base", "initial")

		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "hello", "add feature")
		sha1 := repo.HeadSHA()
		patchID1 := GetPatchID(repo.Dir, sha1)

		assert.NotEmpty(t, patchID1, "expected non-empty patch-id")

		repo.Run("checkout", "main")
		repo.CommitFile("other.txt", "other", "another commit")
		repo.Run("checkout", "feature")
		repo.Run("rebase", "main")
		sha2 := repo.HeadSHA()
		patchID2 := GetPatchID(repo.Dir, sha2)

		assert.NotEqual(t, sha1, sha2, "SHAs should differ after rebase")
		assert.Equal(t, patchID1, patchID2, "patch-ids should match: %s != %s", patchID1, patchID2)
	})

	t.Run("different for modified commits", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.CommitFile("a.txt", "content-a", "commit a")
		sha1 := repo.HeadSHA()

		repo.CommitFile("b.txt", "content-b", "commit b")
		sha2 := repo.HeadSHA()

		pid1 := GetPatchID(repo.Dir, sha1)
		pid2 := GetPatchID(repo.Dir, sha2)

		assert.NotEmpty(t, pid1, "expected non-empty patch-id")
		assert.NotEmpty(t, pid2, "expected non-empty patch-id")
		assert.NotEqual(t, pid1, pid2, "expected distinct patch-ids for different commits")
	})

	t.Run("empty for empty commit", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.CommitFile("a.txt", "content", "first")
		repo.Run("commit", "--allow-empty", "-m", "empty")
		sha := repo.HeadSHA()

		pid := GetPatchID(repo.Dir, sha)
		assert.Empty(t, pid, "expected empty patch-id for empty commit, got %s", pid)
	})
}

func TestShortRef(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"range ref", "abc1234def5678..99887766aabbcc", "abc1234..9988776"},
		{"single sha", "abc1234def5678", "abc1234"},
		{"short single", "abc", "abc"},
		{"empty", "", ""},
		{"range with short sides", "abc..def", "abc..def"},
		{"triple dot splits on first pair", "abc1234def5678...99887766aabbcc", "abc1234...99887766aabbcc"},
		{"task label passthrough", "run", "run"},
		{"dirty ref passthrough", "dirty", "dirty"},
		{"branch name passthrough", "feature/very-long-name", "feature/very-long-name"},
		{"analysis label passthrough", "duplication", "duplication"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShortRef(tt.in)
			assert.Equal(t, tt.want, got, "ShortRef(%q) = %q, want %q", tt.in, got, tt.want)
		})
	}
}

func TestShortSHA(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"full sha", "abc1234def5678", "abc1234"},
		{"exactly 7", "abc1234", "abc1234"},
		{"shorter", "abc", "abc"},
		{"empty", "", ""},
		{"8 chars", "abc12345", "abc1234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShortSHA(tt.in)
			assert.Equal(t, tt.want, got, "ShortSHA(%q) = %q, want %q", tt.in, got, tt.want)
		})
	}
}

func TestWorktreePathForBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	evalSymlinks := func(t *testing.T, path string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(path)
		require.NoError(t, err, "EvalSymlinks(%q): %v", path, err)
		return resolved
	}

	t.Run("returns worktree dir for branch checked out in worktree", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		wt := repo.AddWorktree("feature-x")

		got, checkedOut, err := WorktreePathForBranch(repo.Dir, "feature-x")
		require.NoError(t, err, "unexpected error: %v", err)
		got = evalSymlinks(t, got)
		want := evalSymlinks(t, wt.Dir)
		assert.Equal(t, want, got, "WorktreePathForBranch() path = %q, want %q", got, want)
		assert.True(t, checkedOut, "WorktreePathForBranch() checkedOut = false, want true")
	})

	t.Run("returns repoPath and false when branch has no worktree", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.Run("branch", "other-branch")

		got, checkedOut, err := WorktreePathForBranch(repo.Dir, "other-branch")
		require.NoError(t, err, "unexpected error: %v", err)
		assert.Equal(t, repo.Dir, got, "WorktreePathForBranch() path = %q, want %q", got, repo.Dir)
		assert.False(t, checkedOut, "WorktreePathForBranch() checkedOut = true, want false")
	})

	t.Run("returns repoPath and true for empty branch", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		got, checkedOut, err := WorktreePathForBranch(repo.Dir, "")
		require.NoError(t, err, "unexpected error: %v", err)
		assert.Equal(t, repo.Dir, got, "WorktreePathForBranch() path = %q, want %q", got, repo.Dir)
		assert.True(t, checkedOut, "WorktreePathForBranch() checkedOut = false, want true")
	})

	t.Run("returns main repo dir for branch checked out in main worktree", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		branch := GetCurrentBranch(repo.Dir)

		got, checkedOut, err := WorktreePathForBranch(repo.Dir, branch)
		require.NoError(t, err, "unexpected error: %v", err)
		got = evalSymlinks(t, got)
		want := evalSymlinks(t, repo.Dir)
		assert.Equal(t, want, got, "WorktreePathForBranch() path = %q, want %q", got, want)
		assert.True(t, checkedOut, "WorktreePathForBranch() checkedOut = false, want true")
	})

	t.Run("returns error for invalid repo path", func(t *testing.T) {
		_, _, err := WorktreePathForBranch("/nonexistent/repo", "main")
		require.Error(t, err, "expected error for invalid repo path, got nil")
	})

	t.Run("git worktree add succeeds on pre-existing empty directory", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.Run("branch", "wt-preexist")

		wtDir := t.TempDir()
		repo.Run("worktree", "add", wtDir, "wt-preexist")
		t.Cleanup(func() {
			rmCmd := exec.Command("git", "-C", repo.Dir, "worktree", "remove", wtDir)
			_ = rmCmd.Run()
		})

		_, statErr := os.Stat(filepath.Join(wtDir, "initial.txt"))
		require.NoError(t, statErr, "expected initial.txt in worktree")
	})

	t.Run("skips stale worktree whose directory was deleted", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		wt := repo.AddWorktree("stale-branch")
		wtDir := wt.Dir

		os.RemoveAll(wtDir)

		got, checkedOut, err := WorktreePathForBranch(repo.Dir, "stale-branch")
		require.NoError(t, err, "unexpected error: %v", err)

		if checkedOut {
			_, statErr := os.Stat(got)
			require.NoError(t, statErr, "WorktreePathForBranch() returned checkedOut=true for stale path %q that doesn't exist", got)
		}
	})
}

func TestValidateWorktreeForRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	t.Run("valid worktree passes", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		wt := repo.AddWorktree("val-wt")

		assert.True(t, ValidateWorktreeForRepo(wt.Dir, repo.Dir))
	})

	t.Run("main repo validates against itself", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		assert.True(t, ValidateWorktreeForRepo(repo.Dir, repo.Dir))
	})

	t.Run("nonexistent path fails", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)

		assert.False(t, ValidateWorktreeForRepo("/nonexistent/path", repo.Dir))
	})

	t.Run("unrelated repo fails", func(t *testing.T) {
		repo1 := NewTestRepoWithCommit(t)
		repo2 := NewTestRepoWithCommit(t)

		assert.False(t, ValidateWorktreeForRepo(repo2.Dir, repo1.Dir))
	})

	t.Run("subdirectory of same repo fails", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		subDir := filepath.Join(repo.Dir, "src")
		require.NoError(t, os.MkdirAll(subDir, 0o755))

		assert.False(t, ValidateWorktreeForRepo(subDir, repo.Dir))
	})
}

func TestLooksLikeSHA(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"abc1234", true},
		{"0000000000000000000000000000000000000000", true},
		{"abcdef1234567890abcdef1234567890abcdef12", true},
		{"ABCDEF1", true},         // uppercase hex is valid
		{"AbCd1234", true},        // mixed case
		{"abc123", false},         // too short (6 chars, need 7+)
		{"abcd", false},           // too short (4 chars)
		{"dead", false},           // short hex task label
		{"cafe12", false},         // 6-char hex, still too short
		{"", false},               // empty
		{"dirty", false},          // non-hex
		{"run", false},            // task label
		{"abc123..def456", false}, // range
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			assert.Equal(t, tt.want, LooksLikeSHA(tt.s))
		})
	}
}

func TestGetBranchBase(t *testing.T) {
	t.Run("returns empty when not configured", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		repo.Run("checkout", "-b", "feature")

		assert.Empty(t, GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("returns local bare name when no remote-tracking exists", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "main", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("resolves bare name to origin counterpart when local is stale ancestor", func(t *testing.T) {
		// Reproduces the post-rebase bug: branch.<feature>.base = main with
		// a stale local main pulls extra origin/main commits into the merge-
		// base..HEAD range. The fix translates the bare name to origin/main
		// when local main is just an ancestor of origin/main.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		staleMainSHA := repo.HeadSHA()
		repo.CommitFile("trunk.txt", "trunk", "trunk advance")
		freshOriginSHA := repo.HeadSHA()
		// Rewind local main so it lags one commit behind origin/main.
		repo.Run("update-ref", "refs/heads/main", staleMainSHA)
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", freshOriginSHA)
		repo.Run("checkout", "-b", "feature", freshOriginSHA)
		repo.CommitFile("feature.txt", "f", "feature commit")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "origin/main", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("keeps local name when local has diverged from origin", func(t *testing.T) {
		// Divergence (local main has commits not on origin/main) means the
		// user's local branch is not just stale — respect their config
		// rather than silently picking a different base.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		baseSHA := repo.HeadSHA()
		repo.CommitFile("origin.txt", "origin", "origin-only commit")
		originMainSHA := repo.HeadSHA()
		// Local main diverges with its own commit not present on origin/main.
		repo.Run("update-ref", "refs/heads/main", baseSHA)
		repo.Run("checkout", "main")
		repo.CommitFile("local.txt", "local", "local-only commit")
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", originMainSHA)
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "main", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("returns origin counterpart when local branch is missing", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/scratch")
		repo.CommitFile("base.txt", "base", "initial")
		originSHA := repo.HeadSHA()
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", originSHA)
		// Note: no refs/heads/main. Branch from origin/main directly.
		repo.Run("checkout", "-b", "feature", originSHA)
		repo.CommitFile("feature.txt", "f", "feature commit")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "origin/main", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("returns qualified ref unchanged", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "upstream/main")

		assert.Equal(t, "upstream/main", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("resolves slash-containing local branch when stale", func(t *testing.T) {
		// Branch names may contain slashes (release/1.2, team/main). Such
		// names must still get stale-ancestor translation — treating any
		// '/' as a remote prefix would skip the fix for these cases.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/release/1.2")
		repo.CommitFile("base.txt", "base", "initial")
		staleSHA := repo.HeadSHA()
		repo.CommitFile("trunk.txt", "t", "trunk advance")
		freshSHA := repo.HeadSHA()
		repo.Run("update-ref", "refs/heads/release/1.2", staleSHA)
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/release/1.2", freshSHA)
		repo.Run("checkout", "-b", "feature", freshSHA)
		repo.CommitFile("feature.txt", "f", "feature commit")
		repo.Run("config", "branch.feature.base", "release/1.2")

		assert.Equal(t, "origin/release/1.2", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("passes qualified remote-tracking ref through unchanged", func(t *testing.T) {
		// An unambiguous remote-tracking ref (refs/remotes/<value> exists,
		// no shadowing local branch) is not subject to translation.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		mainSHA := repo.HeadSHA()
		repo.Run("remote", "add", "upstream", "/dev/null")
		repo.Run("update-ref", "refs/remotes/upstream/main", mainSHA)
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "upstream/main")

		assert.Equal(t, "upstream/main", GetBranchBase(repo.Dir, "HEAD"))
	})

	t.Run("does not fall back to origin when configured upstream is missing", func(t *testing.T) {
		// Fork workflow: local main is configured to track upstream/main
		// but upstream/main has not been fetched. origin/main (the user's
		// fork) must NOT be silently substituted — that would compute the
		// merge-base against a different remote than the user intended.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		mainSHA := repo.HeadSHA()
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", mainSHA)
		// Configure local main to track upstream/main, but never set up
		// the corresponding remote-tracking ref.
		repo.Run("config", "branch.main.remote", "upstream")
		repo.Run("config", "branch.main.merge", "refs/heads/main")
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "main", GetBranchBase(repo.Dir, "HEAD"),
			"missing upstream must not be substituted with origin counterpart")
	})

	t.Run("does not collapse remote-qualified value to origin counterpart", func(t *testing.T) {
		// Regression: branch.feature.base = "upstream/main" with the
		// upstream remote-tracking ref absent must not be rewritten to
		// "origin/upstream/main" just because a stray refs/remotes/origin/
		// upstream/main happens to exist. The user explicitly remote-
		// qualified the value; either it resolves or git surfaces an error.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		mainSHA := repo.HeadSHA()
		repo.Run("remote", "add", "origin", "/dev/null")
		// origin holds a stray ref shaped like a remote-qualified path —
		// e.g., a past push of a local branch literally named
		// "upstream/main". refs/remotes/upstream/main remains absent.
		repo.Run("update-ref", "refs/remotes/origin/upstream/main", mainSHA)
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "upstream/main")

		assert.Equal(t, "upstream/main", GetBranchBase(repo.Dir, "HEAD"),
			"explicit remote-qualified value must not collapse to origin/<value>")
	})

	t.Run("does not fall back to origin when configured upstream is local-only", func(t *testing.T) {
		// Local-branch tracking (branch.<name>.remote = ".") explicitly
		// targets another local branch. origin/<name> is unrelated and
		// must not be silently substituted.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/develop")
		repo.CommitFile("base.txt", "base", "initial")
		developSHA := repo.HeadSHA()
		repo.Run("checkout", "-b", "main")
		repo.Run("config", "branch.main.remote", ".")
		repo.Run("config", "branch.main.merge", "refs/heads/develop")
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", developSHA)
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "main", GetBranchBase(repo.Dir, "HEAD"),
			"local-branch tracking must not be substituted with origin counterpart")
	})

	t.Run("prefers configured upstream over origin counterpart", func(t *testing.T) {
		// Fork workflow: local main tracks upstream/main, and origin/main
		// also exists (e.g., points at the user's fork). The base should
		// resolve to upstream/main because that's what local main tracks.
		repo := NewTestRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		mainSHA := repo.HeadSHA()
		repo.CommitFile("trunk.txt", "trunk", "upstream advance")
		upstreamSHA := repo.HeadSHA()
		repo.Run("update-ref", "refs/heads/main", mainSHA)
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("remote", "add", "upstream", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", mainSHA)
		repo.Run("update-ref", "refs/remotes/upstream/main", upstreamSHA)
		repo.Run("config", "branch.main.remote", "upstream")
		repo.Run("config", "branch.main.merge", "refs/heads/main")
		repo.Run("checkout", "-b", "feature", upstreamSHA)
		repo.CommitFile("feature.txt", "f", "feature commit")
		repo.Run("config", "branch.feature.base", "main")

		assert.Equal(t, "upstream/main", GetBranchBase(repo.Dir, "HEAD"))
	})
}

func TestCtxVariantsHonorCancellation(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	sha := strings.TrimSpace(repo.Run("rev-parse", "HEAD"))
	rangeRef := sha + ".." + sha
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{"GetCommitInfoCtx", func() error { _, err := GetCommitInfoCtx(ctx, repo.Dir, sha); return err }},
		{"GetFilesChangedCtx", func() error { _, err := GetFilesChangedCtx(ctx, repo.Dir, sha); return err }},
		{"GetDiffLimitedCtx", func() error { _, _, err := GetDiffLimitedCtx(ctx, repo.Dir, sha, 1024); return err }},
		{"GetRangeCommitsCtx", func() error { _, err := GetRangeCommitsCtx(ctx, repo.Dir, rangeRef); return err }},
		{"GetRangeFilesChangedCtx", func() error { _, err := GetRangeFilesChangedCtx(ctx, repo.Dir, rangeRef); return err }},
		{"GetRangeDiffLimitedCtx", func() error { _, _, err := GetRangeDiffLimitedCtx(ctx, repo.Dir, rangeRef, 1024); return err }},
		{"GetParentCommitsCtx", func() error { _, err := GetParentCommitsCtx(ctx, repo.Dir, sha, 1); return err }},
		{"ResolveSHACtx", func() error { _, err := ResolveSHACtx(ctx, repo.Dir, sha); return err }},
		{"GetRangeStartCtx", func() error { _, err := GetRangeStartCtx(ctx, repo.Dir, rangeRef); return err }},
		{"GetDiffCtx", func() error { _, err := GetDiffCtx(ctx, repo.Dir, sha); return err }},
		{"GetRangeDiffCtx", func() error { _, err := GetRangeDiffCtx(ctx, repo.Dir, rangeRef); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, tt.call(), "canceled context must abort the git call")
		})
	}
}

func TestGetDirtyDiffKataLocalToml(t *testing.T) {
	t.Run("untracked local override is suppressed", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.WriteFile(".kata.local.toml", "[project]\nname = \"local\"\n")
		repo.WriteFile("dirty.txt", "dirty\n")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err)
		assert.Contains(t, diff, "dirty.txt")
		assert.NotContains(t, diff, ".kata.local.toml",
			"an untracked local kata override must not leak into dirty review prompts")
	})

	t.Run("tracked local override modifications stay visible", func(t *testing.T) {
		repo := NewTestRepoWithCommit(t)
		repo.WriteFile(".kata.local.toml", "[project]\nname = \"committed\"\n")
		repo.CommitAll("track kata local override")
		repo.WriteFile(".kata.local.toml", "[project]\nname = \"steered\"\n")

		diff, err := GetDirtyDiff(repo.Dir)
		require.NoError(t, err)
		assert.Contains(t, diff, ".kata.local.toml",
			"modifying a tracked .kata.local.toml must stay visible to dirty reviews")
		assert.Contains(t, diff, "steered")
	})
}
