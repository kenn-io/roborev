package prompt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type testRepo struct {
	t   *testing.T
	dir string
}

const (
	testGitUser  = "Test User"
	testGitEmail = "test@example.com"
)

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	return initTestRepo(t)
}

func newTestRepoWithBranch(t *testing.T, branch string) *testRepo {
	t.Helper()
	return initTestRepo(t, "-b", branch)
}

func initTestRepo(t *testing.T, initArgs ...string) *testRepo {
	t.Helper()
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git(append([]string{"init"}, initArgs...)...)
	r.configure()
	return r
}

func (r *testRepo) configure() {
	r.t.Helper()
	r.git("config", "user.email", testGitEmail)
	r.git("config", "user.name", testGitUser)
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+testGitUser,
		"GIT_AUTHOR_EMAIL="+testGitEmail,
		"GIT_COMMITTER_NAME="+testGitUser,
		"GIT_COMMITTER_EMAIL="+testGitEmail,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %v failed\n%s", args, out)
	return strings.TrimSpace(string(out))
}

func assertContains(t *testing.T, doc, substring, msg string) {
	t.Helper()
	assert.Contains(t, doc, substring, msg)
}

func assertNotContains(t *testing.T, doc, substring, msg string) {
	t.Helper()
	assert.NotContains(t, doc, substring, msg)
}

const testAuthor = "test"

// setupTestRepo creates a git repo with multiple commits and returns the repo path and commit SHAs
func setupTestRepo(t *testing.T) (string, []string) {
	t.Helper()
	r := newTestRepo(t)

	var commits []string

	// Create 6 commits so we can test with 5 previous commits
	for i := 1; i <= 6; i++ {
		filename := filepath.Join(r.dir, "file.txt")
		content := strings.Repeat("x", i) // Different content each time
		require.NoError(t, os.WriteFile(filename, []byte(content), 0o644))
		r.git("add", "file.txt")
		r.git("commit", "-m", "commit "+string(rune('0'+i)))

		sha := r.git("rev-parse", "HEAD")
		commits = append(commits, sha)
	}

	return r.dir, commits
}

func setupLargeDiffRepo(t *testing.T) (string, string) {
	t.Helper()
	r := newTestRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", "base.txt")
	r.git("commit", "-m", "initial")

	var content strings.Builder
	for range 20000 {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("x", 20))
		content.WriteString(" ")
		content.WriteString(strings.Repeat("y", 20))
		content.WriteString("\n")
	}

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "large.txt"),
		[]byte(content.String()), 0o644,
	))
	r.git("add", "large.txt")
	r.git("commit", "-m", "large change")

	return r.dir, r.git("rev-parse", "HEAD")
}

func setupLargeExcludePatternRepo(t *testing.T) (string, string) {
	t.Helper()
	r := newTestRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", "base.txt")
	r.git("commit", "-m", "initial")

	var content strings.Builder
	for range 20000 {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("x", 20))
		content.WriteString(" ")
		content.WriteString(strings.Repeat("y", 20))
		content.WriteString("\n")
	}

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "large.txt"),
		[]byte(content.String()), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "custom.dat"),
		[]byte(content.String()), 0o644,
	))
	r.git("add", "large.txt", "custom.dat")
	r.git("commit", "-m", "large change")

	return r.dir, r.git("rev-parse", "HEAD")
}

func setupLargeDiffRepoWithGuidelines(t *testing.T, guidelineLen int) (string, string) {
	t.Helper()
	r := newTestRepoWithBranch(t, "main")

	guidelines := strings.Repeat("g", guidelineLen)
	toml := `review_guidelines = """` + "\n" + guidelines + "\n" + `"""` + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, ".roborev.toml"),
		[]byte(toml), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", ".roborev.toml", "base.txt")
	r.git("commit", "-m", "initial")

	r.git("remote", "add", "origin", r.dir)
	r.git("fetch", "origin")
	r.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	var content strings.Builder
	for range 20000 {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("x", 20))
		content.WriteString(" ")
		content.WriteString(strings.Repeat("y", 20))
		content.WriteString("\n")
	}

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "large.txt"),
		[]byte(content.String()), 0o644,
	))
	r.git("add", "large.txt")
	r.git("commit", "-m", "large change")

	return r.dir, r.git("rev-parse", "HEAD")
}

func setupLargeCommitBodyRepo(t *testing.T, bodyLen int) (string, string) {
	t.Helper()
	r := newTestRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", "base.txt")
	r.git("commit", "-m", "initial")

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\nnext\n"), 0o644,
	))
	r.git("add", "base.txt")

	msgPath := filepath.Join(r.dir, "commit-message.txt")
	body := strings.Repeat("body line\n", max(1, bodyLen/len("body line\n")))
	message := "large change\n\n" + body
	require.NoError(t, os.WriteFile(msgPath, []byte(message), 0o644))
	r.git("commit", "-F", msgPath)

	return r.dir, r.git("rev-parse", "HEAD")
}

func commitWithRepoConfig(t *testing.T, repoDir, messageFile string) {
	t.Helper()
	cmd := exec.Command("git", "commit", "-F", messageFile)
	cmd.Dir = repoDir
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_AUTHOR_") || strings.HasPrefix(kv, "GIT_COMMITTER_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git commit with repo config failed\n%s", out)
}

func setConfiguredUserName(t *testing.T, repoDir, authorName string) {
	t.Helper()
	configPath := filepath.Join(repoDir, ".git", "config")
	configBytes, err := os.ReadFile(configPath)
	require.NoError(t, err, "read git config: %v", err)

	updated := strings.Replace(string(configBytes), "name = "+testGitUser, "name = "+authorName, 1)
	require.NotEqual(t, string(configBytes), updated, "expected to replace the configured git user name")
	require.NoError(t, os.WriteFile(configPath, []byte(updated), 0o644), "write git config: %v", err)
}

func setupLargeCommitSubjectRepo(t *testing.T, subjectLen int) (string, string) {
	t.Helper()
	r := newTestRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", "base.txt")
	r.git("commit", "-m", "initial")

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\nnext\n"), 0o644,
	))
	r.git("add", "base.txt")

	msgPath := filepath.Join(r.dir, "commit-message.txt")
	message := strings.Repeat("s", subjectLen) + "\n"
	require.NoError(t, os.WriteFile(msgPath, []byte(message), 0o644))
	r.git("commit", "-F", msgPath)

	return r.dir, r.git("rev-parse", "HEAD")
}

func setupLargeCommitAuthorRepo(t *testing.T, authorLen int) (string, string) {
	t.Helper()
	r := newTestRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", "base.txt")
	r.git("commit", "-m", "initial")

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\nnext\n"), 0o644,
	))
	r.git("add", "base.txt")

	msgPath := filepath.Join(r.dir, "commit-message.txt")
	require.NoError(t, os.WriteFile(msgPath, []byte("large change\n"), 0o644))
	setConfiguredUserName(t, r.dir, strings.Repeat("a", authorLen))
	commitWithRepoConfig(t, r.dir, msgPath)

	return r.dir, r.git("rev-parse", "HEAD")
}

func (r *testRepo) commitWithMessage(message string) {
	r.t.Helper()
	msgPath := filepath.Join(r.dir, "commit-message.txt")
	require.NoError(r.t, os.WriteFile(msgPath, []byte(message), 0o644))
	r.git("commit", "--quiet", "-F", msgPath)
	require.NoError(r.t, os.Remove(msgPath))
}

func setupLargeRangeMetadataRepo(t *testing.T, commitCount, subjectLen int) (string, string) {
	t.Helper()
	r := newTestRepo(t)

	require.NoError(t, os.WriteFile(
		filepath.Join(r.dir, "base.txt"),
		[]byte("base\n"), 0o644,
	))
	r.git("add", "base.txt")
	r.git("commit", "-m", "initial")

	startSHA := r.git("rev-parse", "HEAD")
	subject := strings.Repeat("s", subjectLen)
	for i := range commitCount {
		require.NoError(t, os.WriteFile(
			filepath.Join(r.dir, "base.txt"),
			[]byte(strings.Repeat("x\n", i+2)), 0o644,
		))
		r.git("add", "base.txt")
		r.commitWithMessage(subject + "\n")
	}

	endSHA := r.git("rev-parse", "HEAD")
	return r.dir, startSHA + ".." + endSHA
}

func setupDBWithCommits(t *testing.T, repoPath string, commits []string) (*storage.DB, int64) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err, "GetOrCreateRepo failed")
	for _, sha := range commits {
		_, err = db.GetOrCreateCommit(repo.ID, sha, testAuthor, "commit message", time.Now())
		require.NoError(t, err, "GetOrCreateCommit failed")
	}
	return db, repo.ID
}

type guidelinesRepoContext struct {
	Dir        string
	BaseSHA    string
	FeatureSHA string
}

// setupGuidelinesRepo creates a git repo with .roborev.toml on the
// default branch and optionally a feature branch with different
// guidelines. Returns guidelinesRepoContext.
//
// When setupGit is provided, it takes full control of commit creation
// and remote setup (the caller must set up origin/fetch/symbolic-ref
// if needed). This avoids running git fetch on a repo where setupGit
// may have corrupted objects.
func setupGuidelinesRepo(t *testing.T, defaultBranch, baseGuidelines, branchGuidelines string, setupGit func(t *testing.T, r *testRepo)) guidelinesRepoContext {
	t.Helper()
	r := newTestRepoWithBranch(t, defaultBranch)
	if setupGit != nil {
		setupGit(t, r)
		return guidelinesRepoContext{
			Dir:     r.dir,
			BaseSHA: r.git("rev-parse", "HEAD"),
		}
	}

	// Initial commit with base guidelines
	if baseGuidelines != "" {
		toml := `review_guidelines = """` + "\n" + baseGuidelines + "\n" + `"""` + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(r.dir, ".roborev.toml"), []byte(toml), 0o644), "write .roborev.toml")
	} else {
		require.NoError(t, os.WriteFile(filepath.Join(r.dir, "README.md"), []byte("init"), 0o644), "write README.md")
	}
	r.git("add", "-A")
	r.git("commit", "-m", "initial")
	baseSHA := r.git("rev-parse", "HEAD")

	// Set up origin pointing to itself so origin/<branch> exists
	r.git("remote", "add", "origin", r.dir)
	r.git("fetch", "origin")
	// Set origin/HEAD to point to the default branch
	r.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+defaultBranch)

	// Create feature branch with different guidelines
	var featureSHA string
	if branchGuidelines != "" {
		r.git("checkout", "-b", "feature-branch")
		toml := `review_guidelines = """` + "\n" + branchGuidelines + "\n" + `"""` + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(r.dir, ".roborev.toml"), []byte(toml), 0o644), "write .roborev.toml")
		r.git("add", ".roborev.toml")
		r.git("commit", "-m", "update guidelines on branch")
		featureSHA = r.git("rev-parse", "HEAD")
		r.git("checkout", defaultBranch)
	}

	return guidelinesRepoContext{
		Dir:        r.dir,
		BaseSHA:    baseSHA,
		FeatureSHA: featureSHA,
	}
}
