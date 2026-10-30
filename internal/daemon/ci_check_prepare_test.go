package daemon

import (
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

// Preparation checks the committed tree, with relative submodules, and warms modules in a separate checkout.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
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
	original := "module example.com/check\n\ngo 1.25.0\n"
	repo.CommitFile("go.mod", original, "module")

	tools := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
	// go env runs outside the checkout, so a version-manager shim can't load its config.
	outside, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then [ \"$(pwd -P)\" = '%s' ] || exit 4; printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo 'require example.com/upgraded v1.0.0' >> go.mod; exit 1; fi\n", outside, tools, tools)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
	require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
	sha := testutil.GetHeadSHA(t, repo.Root)
	var log bytes.Buffer
	run, err := (&WorkerPool{}).prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, []string{"CHECKER_GIT_TOKEN=synthetic"}, &log)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	assert.Contains(t, log.String(), "module warming failed: exit status 1")
	assert.Contains(t, run.Job.Env, "GOMODCACHE="+tools, log.String())
	assert.Contains(t, run.Job.ReadPaths, tools)
	mod, err := os.ReadFile(filepath.Join(run.Repo, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, original, string(mod))
	git := exec.CommandContext(t.Context(), "git", "-C", filepath.Join(run.Repo, "dependency"), "rev-parse", "HEAD")
	output, err := git.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Equal(t, wantHead, strings.TrimSpace(string(output)))
	content, err := os.ReadFile(filepath.Join(run.Repo, "dependency", "present.txt"))
	require.NoError(t, err)
	assert.Equal(t, "submodule content", string(content))
	// The repository's Git directory joins the read grants, so it can't hold the Codex login.
	t.Setenv("CODEX_HOME", filepath.Join(repo.Root, ".git"))
	_, err = (&WorkerPool{}).prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, io.Discard)
	require.ErrorContains(t, err, "read grant contains host")
}

func TestCICheckWarming(t *testing.T) {
	t.Run("without go", func(t *testing.T) {
		checkout := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n"), 0o600))
		t.Setenv("PATH", t.TempDir())
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, nil, &log)
		require.NoError(t, err)
		assert.Empty(t, paths)
		assert.Contains(t, log.String(), "go env failed")
	})
}

func TestCICheckWarmingMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlinks")
	}
	for _, name := range []string{"go.mod", "go.sum", "go.work", "go.work.sum"} {
		for _, kind := range []string{"symlink", "directory"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				checkout := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n\ngo 1.26.0\n"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.work"), []byte("go 1.26.0\n\nuse .\n"), 0o600))
				path := filepath.Join(checkout, name)
				require.NoError(t, os.RemoveAll(path))
				outside := filepath.Join(t.TempDir(), "metadata")
				require.NoError(t, os.WriteFile(outside, []byte(""), 0o600))
				if kind == "symlink" {
					require.NoError(t, os.Symlink(outside, path))
				} else {
					require.NoError(t, os.Mkdir(path, 0o700))
				}
				_, err := warmGoModules(t.Context(), checkout, os.Environ(), io.Discard)
				require.ErrorContains(t, err, name+" must be a regular file")
			})
		}
	}
}

// Cleanup restores write access only inside checker roots, without following symlinks.
func TestCICheckReadOnlyCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix directory permissions")
	}
	for _, name := range []string{"checker", "checker recovery", "review recovery"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := testutil.NewTestRepoWithCommit(t)
			job := &storage.ReviewJob{RepoPath: repo.Root, GitRef: repo.HeadSHA()}
			wp := &WorkerPool{}
			var checkout, root string
			var cleanup func()
			var opened *os.File
			if name == "review recovery" {
				var err error
				checkout, cleanup, err = wp.createExactCheckout(t.Context(), "worker-1", job)
				require.NoError(t, err)
				dir := filepath.Join(checkout, "readonly")
				require.NoError(t, os.Mkdir(dir, 0o500))
				opened, err = os.Open(dir)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, opened.Close()) })
			} else {
				command := filepath.Join(t.TempDir(), "codex")
				require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
				run, err := wp.prepareCICheck(t.Context(), job, command, nil, io.Discard)
				require.NoError(t, err)
				checkout, root, cleanup = run.Repo, filepath.Dir(run.Job.WorkDir), run.cleanup
				for _, parent := range []string{run.Repo, filepath.Join(run.Job.WorkDir, "gocache"), filepath.Join(run.Job.WorkDir, "tmp")} {
					dir := filepath.Join(parent, "readonly")
					require.NoError(t, os.Mkdir(dir, 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600))
					require.NoError(t, os.Chmod(dir, 0o500))
					t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				}
			}
			t.Cleanup(cleanup)
			outside := t.TempDir()
			require.NoError(t, os.Chmod(outside, 0o500))
			t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
			require.NoError(t, os.Symlink(outside, filepath.Join(checkout, "external")))
			if name == "checker" {
				cleanup()
			} else {
				require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
			}
			if opened != nil {
				info, err := opened.Stat()
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "review checkout recovery changed directory permissions")
			} else {
				assert.NoDirExists(t, root)
			}
			assert.NoDirExists(t, checkout)
			info, err := os.Stat(outside)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
		})
	}
}

func TestCICheckCredentialOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlinks")
	}
	for _, name := range []string{"codex equal", "codex child", "codex symlink", "home child"} {
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
			case "home child":
				home = filepath.Join(tools, "home")
				require.NoError(t, os.Mkdir(home, 0o700))
			}
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", login)
			require.ErrorContains(t, validateCICheckReadPaths([]string{tools}), "read grant contains host")
		})
	}
}
