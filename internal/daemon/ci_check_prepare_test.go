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

// Preparation checks the committed tree and warms modules in a separate checkout.
// Cleanup restores write access only inside checker roots, without following symlinks.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures, symlinks, and Unix directory permissions")
	}
	for _, name := range []string{"checker", "checker recovery", "review recovery"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := testutil.NewTestRepoWithCommit(t)
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
			sha := repo.HeadSHA()
			job := &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}
			wp := &WorkerPool{}
			var checkout, root string
			var cleanup func()
			var opened *os.File
			if name == "review recovery" {
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
				var log bytes.Buffer
				run, err := wp.prepareCICheck(t.Context(), job, command, []string{"CHECKER_GIT_TOKEN=synthetic"}, &log)
				require.NoError(t, err)
				checkout, root, cleanup = run.Repo, filepath.Dir(run.Job.WorkDir), run.cleanup
				assert.Contains(t, log.String(), "module warming failed: exit status 1")
				assert.Contains(t, run.Job.Env, "GOMODCACHE="+tools, log.String())
				assert.Contains(t, run.Job.ReadPaths, tools)
				mod, err := os.ReadFile(filepath.Join(run.Repo, "go.mod"))
				require.NoError(t, err)
				assert.Equal(t, original, string(mod))
				dir := filepath.Join(run.Job.WorkDir, "gocache", "readonly")
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600))
				require.NoError(t, os.Chmod(dir, 0o500))
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				if name == "checker" {
					// The shared object store joins the read grants, so it can't hold the Codex login.
					t.Setenv("CODEX_HOME", filepath.Join(repo.Root, ".git", "objects"))
					_, err = wp.prepareCICheck(t.Context(), job, command, nil, io.Discard)
					require.ErrorContains(t, err, "read grant contains host")
				}
			}
			t.Cleanup(cleanup)
			external := t.TempDir()
			require.NoError(t, os.Chmod(external, 0o500))
			t.Cleanup(func() { _ = os.Chmod(external, 0o700) })
			require.NoError(t, os.Symlink(external, filepath.Join(checkout, "external")))
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
			info, err := os.Stat(external)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
		})
	}
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
		t.Skip("requires symlinks")
	}
	// Preparation covers a login equal to a grant.
	for _, name := range []string{"codex child", "codex symlink", "home child"} {
		t.Run(name, func(t *testing.T) {
			tools := t.TempDir()
			home, login := t.TempDir(), t.TempDir()
			switch name {
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
