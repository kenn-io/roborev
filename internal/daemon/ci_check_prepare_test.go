package daemon

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Preparation runs without nono or Codex. The sandbox copy must hold the
// committed tree: submodule files present, and go.mod and the go.sum link
// unchanged by warming, which must not write through the link to the host.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("requires go for module warming and build")
	}
	proxy := t.TempDir()
	versionDir := filepath.Join(proxy, "example.com", "dependency", "@v")
	require.NoError(t, os.MkdirAll(versionDir, 0o700))
	depMod := "module example.com/dependency\n\ngo 1.25.0\n"
	require.NoError(t, os.WriteFile(filepath.Join(versionDir, "v1.0.0.mod"), []byte(depMod), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(versionDir, "v1.0.0.info"), []byte(`{"Version":"v1.0.0","Time":"2025-01-01T00:00:00Z"}`), 0o600))
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	f, err := z.Create("example.com/dependency@v1.0.0/go.mod")
	require.NoError(t, err)
	_, err = io.WriteString(f, depMod)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	require.NoError(t, os.WriteFile(filepath.Join(versionDir, "v1.0.0.zip"), data.Bytes(), 0o600))
	t.Setenv("GOPROXY", "file://"+proxy)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOFLAGS", "-modcacherw")

	sub := testutil.NewTestRepoWithCommit(t)
	sub.CommitFile("present.txt", "submodule content", "content")
	wantHead := sub.HeadSHA()
	repo := testutil.NewTestRepoWithCommit(t)
	// The upstream and its sibling exist, while no sibling exists next to the checker copy.
	remoteDir := t.TempDir()
	subPath := filepath.Join(remoteDir, "shared.git")
	require.NoError(t, os.Rename(sub.Root, subPath))
	repo.RunGit("remote", "add", "origin", filepath.Join(remoteDir, "upstream.git"))
	repo.RunGit("-c", "protocol.file.allow=always", "submodule", "add", subPath, "dependency")
	repo.RunGit("config", "-f", ".gitmodules", "submodule.dependency.url", "../shared.git")
	repo.RunGit("commit", "-am", "relative submodule")
	original := "module example.com/check\n\ngo 1.24.0\n\nrequire example.com/dependency v1.0.0\n"
	repo.CommitFile("go.mod", original, "module")
	hostPath := filepath.Join(t.TempDir(), "host-checksums")
	require.NoError(t, os.Symlink(hostPath, filepath.Join(repo.Root, "go.sum")))
	repo.RunGit("add", "go.sum")
	repo.RunGit("commit", "-m", "checksum link")

	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("native fixture"), 0o700))
	sha := testutil.GetHeadSHA(t, repo.Root)
	wp := &WorkerPool{}
	run, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, io.Discard)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	git := exec.CommandContext(t.Context(), "git", "-C", filepath.Join(run.Repo, "dependency"), "rev-parse", "HEAD")
	output, err := git.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Equal(t, wantHead, strings.TrimSpace(string(output)))
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(t.TempDir(), "copy"), ".")
	build.Dir = run.Repo
	output, err = build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	content, err := os.ReadFile(filepath.Join(run.Repo, "dependency", "present.txt"))
	require.NoError(t, err)
	assert.Equal(t, "submodule content", string(content))
	mod, err := os.ReadFile(filepath.Join(run.Repo, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, original, string(mod))
	target, err := os.Readlink(filepath.Join(run.Repo, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, hostPath, target)
	assert.NoFileExists(t, hostPath)
}

func TestCICheckWarming(t *testing.T) {
	checkout := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n"), 0o600))
	t.Run("without go", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, nil, &log)
		require.NoError(t, err)
		assert.Empty(t, paths)
		assert.Contains(t, log.String(), "go env failed")
	})
	t.Run("download fails", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires shell fixtures")
		}
		tools := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo attempted > warmed; exit 1; fi\n", tools, tools)
		require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
		require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
		t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, append(os.Environ(), "CHECKER_GIT_TOKEN=synthetic"), &log)
		require.NoError(t, err)
		assert.Equal(t, []string{tools, tools}, paths)
		assert.FileExists(t, filepath.Join(checkout, "warmed"))
		assert.Contains(t, log.String(), "module warming failed")
	})
}

func TestCICheckNestedModules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	for _, name := range []string{"backend/go.mod", "tools/go.mod", "workspace/go.work"} {
		repo.CommitFile(name, "module example.com/check\n", "Go metadata")
	}
	tools := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo attempted > warmed; fi\n", tools, tools)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
	require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	var log bytes.Buffer
	paths, err := warmGoModules(t.Context(), repo.Root, append(os.Environ(), "CHECKER_GIT_TOKEN=synthetic"), &log)
	require.NoError(t, err)
	assert.Equal(t, []string{tools, tools}, paths)
	for _, dir := range []string{"backend", "tools", "workspace"} {
		assert.FileExists(t, filepath.Join(repo.Root, dir, "warmed"))
	}
	assert.Empty(t, log.String())
}

func TestCICheckDisabledCleanupPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permissions")
	}
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := testutil.NewTestRepoWithCommit(t)
			wp := &WorkerPool{}
			checkout, cleanup, err := wp.createExactCheckout(t.Context(), "worker-1", &storage.ReviewJob{RepoPath: repo.Root, GitRef: repo.HeadSHA()})
			require.NoError(t, err)
			t.Cleanup(cleanup)
			dir := filepath.Join(checkout, "readonly")
			require.NoError(t, os.Mkdir(dir, 0o500))
			opened, err := os.Open(dir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, opened.Close()) })
			if recovery {
				require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
			} else {
				cleanup()
			}
			assert.NoDirExists(t, checkout)
			info, err := opened.Stat()
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "checker-off cleanup changed directory permissions")
		})
	}
}

func TestCICheckRestartRecovery(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("native fixture"), 0o700))
	wp := &WorkerPool{}
	run, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: repo.HeadSHA()}, command, nil, io.Discard)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	root := filepath.Dir(run.Job.WorkDir)
	require.NoError(t, os.WriteFile(filepath.Join(run.Job.WorkDir, "gocache", "cached"), []byte("cache"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(run.Job.WorkDir, "scratch"), []byte("scratch"), 0o600))
	before, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "worktree", "list", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	require.Contains(t, normalizeGitListOutput(string(before)), normalizeGitListPath(t, run.Repo))

	// Startup recovery runs after an abrupt exit, without normal run cleanup.
	require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
	assert.NoDirExists(t, run.Repo)
	assert.NoDirExists(t, root)
	after, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "worktree", "list", "--porcelain").CombinedOutput()
	require.NoError(t, err)
	assert.NotContains(t, normalizeGitListOutput(string(after)), normalizeGitListPath(t, run.Repo))
}

func TestCICheckWarmingIgnoresMiseConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires mise shims and shell scripts")
	}
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("requires mise")
	}
	control := exec.CommandContext(t.Context(), "go", "env", "GOROOT", "GOMODCACHE")
	control.Dir = os.TempDir()
	data, err := control.Output()
	require.NoError(t, err)
	wantPaths := strings.Split(strings.TrimSpace(string(data)), "\n")
	checkout := t.TempDir()
	root := filepath.Join(checkout, "checker")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "mise.toml"), []byte("[env]\n_.source = 'env.sh'\n"), 0o600))
	marker := filepath.Join(t.TempDir(), "sourced")
	t.Setenv("CHECKER_SOURCE_MARKER", marker)
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "env.sh"), []byte("echo sourced > \"$CHECKER_SOURCE_MARKER\"\nexit 23\n"), 0o600))
	shims := t.TempDir()
	require.NoError(t, os.Symlink(mise, filepath.Join(shims, "go")))
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", checkout)
	// Prove the installed shim loads the checkout's script before Go runs.
	probe := exec.CommandContext(t.Context(), "go", "env", "GOROOT")
	probe.Dir = root
	_, err = probe.CombinedOutput()
	require.Error(t, err)
	require.FileExists(t, marker)
	require.NoError(t, os.Remove(marker))

	var log bytes.Buffer
	paths, err := warmGoModules(t.Context(), checkout, nil, &log)
	require.NoError(t, err)
	assert.NoFileExists(t, marker, "Go preparation sourced checkout configuration")
	assert.Equal(t, wantPaths, paths, log.String())
	assert.Empty(t, log.String())
}

func TestCICheckReadOnlyCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permissions")
	}
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := testutil.NewTestRepoWithCommit(t)
			command := filepath.Join(t.TempDir(), "codex")
			require.NoError(t, os.WriteFile(command, []byte("native fixture"), 0o700))
			wp := &WorkerPool{}
			run, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: repo.HeadSHA()}, command, nil, io.Discard)
			require.NoError(t, err)
			t.Cleanup(run.cleanup)
			for _, parent := range []string{run.Repo, filepath.Join(run.Job.WorkDir, "gocache"), filepath.Join(run.Job.WorkDir, "tmp")} {
				dir := filepath.Join(parent, "readonly")
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600))
				require.NoError(t, os.Chmod(dir, 0o500))
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			}
			outside := t.TempDir()
			require.NoError(t, os.Chmod(outside, 0o500))
			t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
			require.NoError(t, os.Symlink(outside, filepath.Join(run.Repo, "external")))
			if recovery {
				require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
			} else {
				run.cleanup()
			}
			assert.NoDirExists(t, filepath.Dir(run.Job.WorkDir))
			assert.NoDirExists(t, run.Repo)
			info, err := os.Stat(outside)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
			output, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "worktree", "list", "--porcelain").CombinedOutput()
			require.NoError(t, err)
			assert.NotContains(t, normalizeGitListOutput(string(output)), normalizeGitListPath(t, run.Repo))
		})
	}
}

func TestCICheckCredentialOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlinks")
	}
	for _, name := range []string{"codex equal", "codex child", "codex symlink", "home equal", "home child", "git directory", "no overlap"} {
		t.Run(name, func(t *testing.T) {
			tools := t.TempDir()
			home, login := t.TempDir(), t.TempDir()
			switch name {
			case "codex equal":
				login = tools
			case "codex child", "codex symlink":
				login = filepath.Join(tools, "login")
				require.NoError(t, os.Mkdir(login, 0o700))
				if name == "codex symlink" {
					link := filepath.Join(t.TempDir(), "login")
					require.NoError(t, os.Symlink(login, link))
					login = link
				}
			case "home equal":
				home = tools
			case "home child":
				home = filepath.Join(tools, "home")
				require.NoError(t, os.Mkdir(home, 0o700))
			}
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", login)
			if name == "git directory" {
				command := filepath.Join(tools, "codex")
				require.NoError(t, os.WriteFile(command, []byte("native fixture"), 0o700))
				tc := newWorkerTestContext(t, 1)
				t.Setenv("CODEX_HOME", filepath.Join(tc.TmpDir, ".git"))
				sha := testutil.GetHeadSHA(t, tc.TmpDir)
				run, err := tc.Pool.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: tc.TmpDir, GitRef: sha + ".." + sha}, command, nil, io.Discard)
				if err == nil {
					t.Cleanup(run.cleanup)
				}
				require.ErrorContains(t, err, "read grant contains host")
				return
			}
			err := validateCICheckReadPaths([]string{tools})
			if name == "no overlap" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "read grant contains host")
			}
		})
	}
}
