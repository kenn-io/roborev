package testutil

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestRepo encapsulates a temporary git repository for tests.
type TestRepo struct {
	Root         string
	GitDir       string
	HooksDir     string
	HookPath     string
	resolvedPath string
	t            *testing.T
}

const mainGoContent = "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"

// Creating a git repository costs several git subprocess spawns (init, config,
// add, commit, ...). On Windows each spawn is ~50ms, and the suite builds 160+
// repos, so that setup dominates Windows test time. Instead we build each
// distinct repo "shape" exactly once per test process and copy the resulting
// directory tree (a few small files) into each test's own temp dir. Copying is
// ~9x faster than re-running git per repo and spawns no processes at all.

var (
	templateMu   sync.Mutex
	templateDirs = map[string]string{}
)

// templateFor returns the path to a cached template repository for key,
// building it once via build the first time it is requested. The template
// persists for the process lifetime (the OS reclaims the temp dir); it is only
// ever read from after creation, so concurrent callers safely share it.
func templateFor(key string, build func(dir string)) string {
	templateMu.Lock()
	defer templateMu.Unlock()
	if dir, ok := templateDirs[key]; ok {
		return dir
	}
	dir, err := os.MkdirTemp("", "roborev-tmpl-"+key+"-*")
	if err != nil {
		panic("testutil: create template dir: " + err.Error())
	}
	build(dir)
	templateDirs[key] = dir
	return dir
}

// mustGit runs a git command for template construction, panicking on failure.
// Templates are built once and a failure is unrecoverable, so a panic (rather
// than a *testing.T error) keeps this usable from the cached, test-independent
// build path.
func mustGit(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("testutil: template git %v failed: %v\n%s", args, err, out))
	}
}

func mustWrite(dir, name, content string) {
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(fmt.Sprintf("testutil: mkdir for %q: %v", name, err))
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(fmt.Sprintf("testutil: write %q: %v", name, err))
	}
}

func configureTemplateUser(dir string) {
	mustGit(dir, "config", "user.email", GitUserEmail)
	mustGit(dir, "config", "user.name", GitUserName)
	mustGit(dir, "config", "gc.auto", "0")
	mustGit(dir, "config", "maintenance.auto", "false")
}

// copyTree recursively copies the contents of src into dst. Files are written
// 0644 and directories 0755 regardless of source mode: git does not require its
// object files to stay read-only, and writable copies avoid read-only-file
// removal failures during test cleanup on Windows.
func copyTree(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// instantiateInto copies the template identified by key into dir.
func instantiateInto(t *testing.T, dir, key string, build func(dir string)) {
	t.Helper()
	tmpl := templateFor(key, build)
	if err := copyTree(dir, tmpl); err != nil {
		t.Fatalf("instantiate template %s into %q: %v", key, dir, err)
	}
}

// newRepoFromTemplate builds a TestRepo rooted at a fresh temp dir populated
// from the named template.
func newRepoFromTemplate(t *testing.T, key string, resolvePath bool, build func(dir string)) *TestRepo {
	t.Helper()
	dir := t.TempDir()
	instantiateInto(t, dir, key, build)

	repo := &TestRepo{
		Root:     dir,
		GitDir:   filepath.Join(dir, ".git"),
		HooksDir: filepath.Join(dir, ".git", "hooks"),
		HookPath: filepath.Join(dir, ".git", "hooks", "post-commit"),
		t:        t,
	}
	if resolvePath {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			resolved = dir
		}
		repo.resolvedPath = resolved
	}
	return repo
}

func runGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}

	return strings.TrimSpace(string(out))
}

func (r *TestRepo) runGitWithEnv(env []string, args ...string) string {
	r.t.Helper()
	return runGit(r.t, r.Root, env, args...)
}

func (r *TestRepo) Run(args ...string) string {
	r.t.Helper()
	return r.runGitWithEnv(nil, args...)
}

// NewTestRepo creates a temporary git repository.
func NewTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	return newRepoFromTemplate(t, "init", false, func(dir string) {
		mustGit(dir, "init")
	})
}

// NewTestRepoWithCommit creates a temporary git repository with a file and
// initial commit, suitable for tests that need a valid git history.
func NewTestRepoWithCommit(t *testing.T) *TestRepo {
	t.Helper()
	return newRepoFromTemplate(t, "with-commit", false, func(dir string) {
		mustGit(dir, "init")
		configureTemplateUser(dir)
		mustWrite(dir, "main.go", mainGoContent)
		mustGit(dir, "add", "main.go")
		mustGit(dir, "commit", "-m", "initial commit")
	})
}

// InitTestRepo creates a standard test repository with an initial commit on the main branch.
func InitTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	return newRepoFromTemplate(t, "main-base", false, func(dir string) {
		mustGit(dir, "init")
		configureTemplateUser(dir)
		mustGit(dir, "symbolic-ref", "HEAD", "refs/heads/main")
		mustWrite(dir, "base.txt", "base")
		mustGit(dir, "add", "base.txt")
		mustGit(dir, "commit", "-m", "base commit")
	})
}

func (r *TestRepo) Path() string {
	if r.resolvedPath != "" {
		return r.resolvedPath
	}
	return r.Root
}

func (r *TestRepo) HeadSHA() string {
	r.t.Helper()
	if sha, err := headSHA(r.Root); err == nil {
		return sha
	}
	return r.RevParse("HEAD")
}

func (r *TestRepo) resolveRevision(repo *gogit.Repository, rev string) plumbing.Hash {
	r.t.Helper()
	resolved, err := repo.ResolveRevision(plumbing.Revision(rev))
	require.NoError(r.t, err, "resolve ref %q", rev)
	return *resolved
}

// RunGit runs a git command in the repo directory.
func (r *TestRepo) RunGit(args ...string) {
	r.t.Helper()
	r.Run(args...)
}

// RevParse runs git rev-parse and returns the trimmed output.
func (r *TestRepo) RevParse(args ...string) string {
	r.t.Helper()
	return r.Run(append([]string{"rev-parse"}, args...)...)
}

// WriteFile writes a file relative to the repo root, creating parent directories as needed.
func (r *TestRepo) WriteFile(name, content string) {
	r.t.Helper()

	path := filepath.Join(r.Root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// CommitFile writes a file, stages it, commits, and returns the new HEAD SHA.
// The stage+commit is done in-process with go-git rather than two `git`
// subprocesses: this helper is called hundreds of times across the suite, and
// on Windows each spawn costs ~50ms. Test repos are always plain checkouts (not
// linked worktrees), so go-git handles them correctly.
func (r *TestRepo) CommitFile(filename, content, msg string) string {
	r.t.Helper()

	r.WriteFile(filename, content)
	return r.commitPaths(msg, filename)
}

// CommitFiles writes files, stages them, commits, and returns the new HEAD SHA.
func (r *TestRepo) CommitFiles(files map[string]string, msg string) string {
	r.t.Helper()
	paths := make([]string, 0, len(files))
	for name, content := range files {
		r.WriteFile(name, content)
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return r.commitPaths(msg, paths...)
}

// CommitEmpty creates an empty commit with the given message and returns HEAD.
func (r *TestRepo) CommitEmpty(msg string) string {
	r.t.Helper()

	repo, err := gogit.PlainOpen(r.Root)
	if err != nil {
		r.t.Fatalf("open repo %q: %v", r.Root, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		r.t.Fatalf("worktree: %v", err)
	}
	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		AllowEmptyCommits: true,
		Author:            testSignature(),
	})
	if err != nil {
		r.t.Fatalf("empty commit: %v", err)
	}
	return hash.String()
}

// UnrelatedCommit writes an unreferenced root commit object using the current
// HEAD tree. It is useful for ancestry tests that need real but unreachable
// commits without paying for orphan-branch checkout/reset subprocesses.
func (r *TestRepo) UnrelatedCommit(msg string) string {
	r.t.Helper()

	repo, err := gogit.PlainOpen(r.Root)
	if err != nil {
		r.t.Fatalf("open repo %q: %v", r.Root, err)
	}
	head, err := repo.Head()
	if err != nil {
		r.t.Fatalf("read HEAD: %v", err)
	}
	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		r.t.Fatalf("read HEAD commit: %v", err)
	}
	sig := testSignature()
	commit := &object.Commit{
		Author:    *sig,
		Committer: *sig,
		Message:   msg,
		TreeHash:  headCommit.TreeHash,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		r.t.Fatalf("encode unrelated commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		r.t.Fatalf("store unrelated commit: %v", err)
	}
	return hash.String()
}

func (r *TestRepo) commitPaths(msg string, paths ...string) string {
	r.t.Helper()

	repo, err := gogit.PlainOpen(r.Root)
	if err != nil {
		r.t.Fatalf("open repo %q: %v", r.Root, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		r.t.Fatalf("worktree: %v", err)
	}
	for _, path := range paths {
		if _, err := wt.Add(filepath.ToSlash(path)); err != nil {
			r.t.Fatalf("git add %s: %v", path, err)
		}
	}
	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: testSignature(),
	})
	if err != nil {
		r.t.Fatalf("commit: %v", err)
	}
	return hash.String()
}

func testSignature() *object.Signature {
	return &object.Signature{Name: GitUserName, Email: GitUserEmail, When: time.Now()}
}

func headSHA(dir string) (string, error) {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a plain .git directory", filepath.Join(dir, ".git"))
	}
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	return head.Hash().String(), nil
}

// CheckoutNewBranch creates and checks out branch at the current HEAD or the
// optional starting ref, without spawning a git process.
func (r *TestRepo) CheckoutNewBranch(branch string, start ...string) {
	r.t.Helper()
	if len(start) > 1 {
		r.t.Fatalf("CheckoutNewBranch accepts at most one start ref")
	}
	repo, err := gogit.PlainOpen(r.Root)
	if err != nil {
		r.t.Fatalf("open repo %q: %v", r.Root, err)
	}
	var hash plumbing.Hash
	if len(start) == 1 {
		hash = r.resolveRevision(repo, start[0])
	} else {
		head, err := repo.Head()
		if err != nil {
			r.t.Fatalf("read HEAD: %v", err)
		}
		hash = head.Hash()
	}
	wt, err := repo.Worktree()
	if err != nil {
		r.t.Fatalf("worktree: %v", err)
	}
	err = wt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: true,
		Hash:   hash,
	})
	if err != nil {
		r.t.Fatalf("checkout new branch %q: %v", branch, err)
	}
}

// CheckoutBranch checks out an existing branch without spawning a git process.
func (r *TestRepo) CheckoutBranch(branch string) {
	r.t.Helper()
	repo, err := gogit.PlainOpen(r.Root)
	require.NoError(r.t, err, "open repo %q", r.Root)
	wt, err := repo.Worktree()
	require.NoError(r.t, err, "worktree")
	err = wt.Checkout(&gogit.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Force:  true,
	})
	require.NoError(r.t, err, "checkout branch %q", branch)
}

// CheckoutBranchForce creates or resets branch to start, then checks it out.
func (r *TestRepo) CheckoutBranchForce(branch string, start ...string) {
	r.t.Helper()
	require.LessOrEqual(r.t, len(start), 1, "CheckoutBranchForce accepts at most one start ref")
	repo, err := gogit.PlainOpen(r.Root)
	require.NoError(r.t, err, "open repo %q", r.Root)
	var hash plumbing.Hash
	if len(start) == 1 {
		hash = r.resolveRevision(repo, start[0])
	} else {
		head, err := repo.Head()
		require.NoError(r.t, err, "read HEAD")
		hash = head.Hash()
	}
	ref := plumbing.NewBranchReferenceName(branch)
	err = repo.Storer.SetReference(plumbing.NewHashReference(ref, hash))
	require.NoError(r.t, err, "set branch %q", branch)
	wt, err := repo.Worktree()
	require.NoError(r.t, err, "worktree")
	err = wt.Checkout(&gogit.CheckoutOptions{Branch: ref, Force: true})
	require.NoError(r.t, err, "checkout branch %q", branch)
}

// CheckoutDetached detaches HEAD at start, or at the current HEAD when omitted.
func (r *TestRepo) CheckoutDetached(start ...string) {
	r.t.Helper()
	require.LessOrEqual(r.t, len(start), 1, "CheckoutDetached accepts at most one start ref")
	repo, err := gogit.PlainOpen(r.Root)
	require.NoError(r.t, err, "open repo %q", r.Root)
	var hash plumbing.Hash
	if len(start) == 1 {
		hash = r.resolveRevision(repo, start[0])
	} else {
		head, err := repo.Head()
		require.NoError(r.t, err, "read HEAD")
		hash = head.Hash()
	}
	wt, err := repo.Worktree()
	require.NoError(r.t, err, "worktree")
	err = wt.Checkout(&gogit.CheckoutOptions{Hash: hash, Force: true})
	require.NoError(r.t, err, "checkout detached %q", hash.String())
}

// AmendCommit stages paths, amends HEAD, and returns the new HEAD SHA.
func (r *TestRepo) AmendCommit(msg string, paths ...string) string {
	r.t.Helper()
	repo, err := gogit.PlainOpen(r.Root)
	require.NoError(r.t, err, "open repo %q", r.Root)
	wt, err := repo.Worktree()
	require.NoError(r.t, err, "worktree")
	for _, path := range paths {
		_, err := wt.Add(filepath.ToSlash(path))
		require.NoError(r.t, err, "git add %s", path)
	}
	hash, err := wt.Commit(msg, &gogit.CommitOptions{
		Amend:  true,
		Author: testSignature(),
	})
	require.NoError(r.t, err, "amend commit")
	return hash.String()
}

// SetRef writes a hash ref directly.
func (r *TestRepo) SetRef(ref, sha string) {
	r.t.Helper()
	repo, err := gogit.PlainOpen(r.Root)
	if err != nil {
		r.t.Fatalf("open repo %q: %v", r.Root, err)
	}
	err = repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.ReferenceName(ref),
		plumbing.NewHash(sha),
	))
	if err != nil {
		r.t.Fatalf("set ref %q: %v", ref, err)
	}
}

func (r *TestRepo) AddRemote(name, url string) {
	r.t.Helper()
	r.appendConfig("remote", name, map[string]string{
		"url":   url,
		"fetch": "+refs/heads/*:refs/remotes/" + name + "/*",
	})
}

func (r *TestRepo) SetRemoteHead(remote, branch string) {
	r.t.Helper()
	r.writeGitFile("refs/remotes/"+remote+"/HEAD", "ref: refs/remotes/"+remote+"/"+branch+"\n")
}

func (r *TestRepo) SetBranchConfig(branch, key, value string) {
	r.t.Helper()
	r.appendConfig("branch", branch, map[string]string{key: value})
}

func (r *TestRepo) writeGitFile(relPath, content string) {
	r.t.Helper()
	path := filepath.Join(r.GitDir, filepath.FromSlash(relPath))
	require.NoError(r.t, os.MkdirAll(filepath.Dir(path), 0o755), "create git dir for %s", relPath)
	require.NoError(r.t, os.WriteFile(path, []byte(content), 0o644), "write git file %s", relPath)
}

func (r *TestRepo) appendConfig(section, subsection string, values map[string]string) {
	r.t.Helper()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteByte('\n')
	if subsection == "" {
		fmt.Fprintf(&b, "[%s]\n", section)
	} else {
		fmt.Fprintf(&b, "[%s %q]\n", section, subsection)
	}
	for _, key := range keys {
		fmt.Fprintf(&b, "\t%s = %s\n", key, values[key])
	}

	f, err := os.OpenFile(filepath.Join(r.GitDir, "config"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(r.t, err, "open git config")
	defer f.Close()
	_, err = f.WriteString(b.String())
	require.NoError(r.t, err, "write git config")
}

// Config sets a git config value.
func (r *TestRepo) Config(key, value string) {
	r.t.Helper()
	r.RunGit("config", key, value)
}

// Checkout runs git checkout.
func (r *TestRepo) Checkout(args ...string) {
	r.t.Helper()
	allArgs := append([]string{"checkout"}, args...)
	r.RunGit(allArgs...)
}

// SymbolicRef runs git symbolic-ref.
func (r *TestRepo) SymbolicRef(ref, target string) {
	r.t.Helper()
	r.RunGit("symbolic-ref", ref, target)
}

func NewGitRepo(t *testing.T) *TestRepo {
	t.Helper()
	return newRepoFromTemplate(t, "init-main", true, func(dir string) {
		mustGit(dir, "init", "-b", "main")
		configureTemplateUser(dir)
	})
}

// NewBareTestRepo creates a bare temporary repository suitable for use as a
// test remote. The bare repository shape is copied from a cached template to
// avoid paying for `git init --bare` in every test case.
func NewBareTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	dir := t.TempDir()
	instantiateInto(t, dir, "bare", func(d string) {
		mustGit(d, "init", "--bare")
	})
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = dir
	}
	return &TestRepo{
		Root:         dir,
		GitDir:       dir,
		HooksDir:     filepath.Join(dir, "hooks"),
		HookPath:     filepath.Join(dir, "hooks", "post-commit"),
		resolvedPath: resolved,
		t:            t,
	}
}

// InitTestGitRepo initializes a git repository with a commit in the given directory.
// Creates the directory if it doesn't exist, runs git init, configures user, creates
// a test file, and makes an initial commit.
func InitTestGitRepo(t *testing.T, dir string) *TestRepo {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create repo dir %q: %v", dir, err)
	}
	instantiateInto(t, dir, "test-commit", func(d string) {
		mustGit(d, "init")
		configureTemplateUser(d)
		mustWrite(d, "test.txt", "test content")
		mustGit(d, "add", "test.txt")
		mustGit(d, "commit", "-m", "initial commit")
	})
	return &TestRepo{
		Root:     dir,
		GitDir:   filepath.Join(dir, ".git"),
		HooksDir: filepath.Join(dir, ".git", "hooks"),
		HookPath: filepath.Join(dir, ".git", "hooks", "post-commit"),
		t:        t,
	}
}

// GetHeadSHA returns the HEAD commit SHA for the git repo at dir.
func GetHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	if sha, err := headSHA(dir); err == nil {
		return sha
	}
	return runGit(t, dir, nil, "rev-parse", "HEAD")
}
