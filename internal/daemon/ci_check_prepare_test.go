package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Preparation warms modules in a separate checkout.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("go.mod", "module example.com/check\n\ngo 1.25.0\n", "module")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	repo.WriteFile(".githooks/post-checkout", fmt.Sprintf("#!/bin/sh\nprintf 'hook ran' > '%s'\n", marker))
	require.NoError(t, os.Chmod(filepath.Join(repo.Root, ".githooks", "post-checkout"), 0o700))
	repo.RunGit("add", ".githooks/post-checkout")
	repo.RunGit("commit", "-m", "checkout hook fixture")
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[core]\n\thooksPath = .githooks\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	tools := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
	// go env runs outside the checkout, so a version-manager shim can't load its config.
	outside, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then [ \"$(pwd -P)\" = '%s' ] || exit 4; printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo 'require example.com/upgraded v1.0.0' >> go.mod; exit 1; fi\n", outside, tools, tools)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
	require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	sha := repo.HeadSHA()
	job := &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}
	wp := &WorkerPool{}
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
	var log bytes.Buffer
	run, err := wp.prepareCICheck(t.Context(), job, command, []string{"CHECKER_GIT_TOKEN=synthetic"}, &log)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	assert.NoFileExists(t, marker, "checker preparation must leave repository hooks unexecuted on the host")
	assert.Contains(t, log.String(), "module warming failed: exit status 1")
	assert.Contains(t, run.Job.Env, "GOMODCACHE="+tools, log.String())
	assert.Contains(t, run.Job.ReadPaths, tools)
	module, err := os.ReadFile(filepath.Join(run.Repo, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, "module example.com/check\n\ngo 1.25.0\n", string(module))
}

// Recovery restores write access inside checker roots without following symlinks.
func TestCICheckRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlinks and Unix directory permissions")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.HeadSHA()
	wp := &WorkerPool{}
	checkout, closeCheckout, err := wp.createExactCheckout(t.Context(), "ci-checker", &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha})
	require.NoError(t, err)
	t.Cleanup(ciCheckCleanup(checkout, closeCheckout))
	root, err := os.MkdirTemp(checkout, "roborev-ci-check-")
	require.NoError(t, err)
	workDir := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "gocache"), 0o700))
	dir := filepath.Join(workDir, "gocache", "readonly")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	external := t.TempDir()
	require.NoError(t, os.Chmod(external, 0o500))
	t.Cleanup(func() { _ = os.Chmod(external, 0o700) })
	require.NoError(t, os.Symlink(external, filepath.Join(root, "external")))
	require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
	assert.NoDirExists(t, root)
	assert.NoDirExists(t, checkout)
	info, err := os.Stat(external)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
}

// Host-side warming refuses metadata that isn't a regular file, skips hosts without Go, and creates a missing module cache.
func TestCICheckWarming(t *testing.T) {
	metadata := []string{"go.mod", "go.sum", "go.work", "go.work.sum"}
	newCheckout := func(t *testing.T) string {
		checkout := t.TempDir()
		for _, name := range metadata {
			require.NoError(t, os.WriteFile(filepath.Join(checkout, name), []byte("go 1.26.0\n"), 0o600))
		}
		return checkout
	}
	for _, name := range metadata {
		for _, kind := range []string{"symlink", "directory"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				if runtime.GOOS == "windows" {
					t.Skip("requires symlinks")
				}
				path := filepath.Join(newCheckout(t), name)
				require.NoError(t, os.Remove(path))
				if kind == "symlink" {
					outside := filepath.Join(t.TempDir(), "metadata")
					require.NoError(t, os.WriteFile(outside, nil, 0o600))
					require.NoError(t, os.Symlink(outside, path))
				} else {
					require.NoError(t, os.Mkdir(path, 0o700))
				}
				_, err := warmGoModules(t.Context(), filepath.Dir(path), nil, io.Discard)
				require.ErrorContains(t, err, name+" must be a regular file")
			})
		}
	}
	t.Run("without go", func(t *testing.T) {
		checkout := newCheckout(t)
		t.Setenv("PATH", t.TempDir())
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, nil, &log)
		require.NoError(t, err)
		assert.Empty(t, paths)
		assert.Contains(t, log.String(), "go env failed")
	})
	t.Run("without dependencies", func(t *testing.T) {
		cache := filepath.Join(t.TempDir(), "missing", "mod")
		t.Setenv("GOMODCACHE", cache)
		t.Setenv("GOTOOLCHAIN", "local")
		t.Setenv("GOWORK", "off")
		checkout := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n\ngo 1.25.0\n"), 0o600))
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, os.Environ(), &log)
		require.NoError(t, err)
		require.Len(t, paths, 2, log.String())
		assert.Equal(t, cache, paths[1])
		assert.DirExists(t, paths[1], "the sandbox must be able to grant the module cache")
	})
}

func TestCICheckCredentialOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir reads USERPROFILE on Windows")
	}
	tools := t.TempDir()
	home := filepath.Join(tools, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv("HOME", home)
	require.ErrorContains(t, validateCICheckReadPaths([]string{tools}), "read grant contains host")
}
