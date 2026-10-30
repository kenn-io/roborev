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

// Preparation preserves the committed tree and relative submodule contents.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("requires go for module warming")
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

	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
	sha := testutil.GetHeadSHA(t, repo.Root)
	wp := &WorkerPool{}
	run, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, io.Discard)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	git := exec.CommandContext(t.Context(), "git", "-C", filepath.Join(run.Repo, "dependency"), "rev-parse", "HEAD")
	output, err := git.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Equal(t, wantHead, strings.TrimSpace(string(output)))
	content, err := os.ReadFile(filepath.Join(run.Repo, "dependency", "present.txt"))
	require.NoError(t, err)
	assert.Equal(t, "submodule content", string(content))
	mod, err := os.ReadFile(filepath.Join(run.Repo, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, original, string(mod))
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
	t.Run("nonregular module", func(t *testing.T) {
		checkout := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(checkout, "go.mod"), 0o700))
		_, err := warmGoModules(t.Context(), checkout, nil, io.Discard)
		require.ErrorContains(t, err, "root go.mod must be a regular file")
	})

	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("download fails=%t", fails), func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("requires shell fixtures")
			}
			repo := testutil.NewTestRepoWithCommit(t)
			repo.CommitFile("go.mod", "module example.com/check\n\ngo 1.25.0\n", "Go metadata")
			tools := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
			exitCode := 0
			if fails {
				exitCode = 1
			}
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo attempted > warmed; exit %d; fi\n", tools, tools, exitCode)
			require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
			require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			var log bytes.Buffer
			paths, err := warmGoModules(t.Context(), repo.Root, append(os.Environ(), "CHECKER_GIT_TOKEN=synthetic"), &log)
			require.NoError(t, err)
			assert.Equal(t, []string{tools, tools}, paths)
			assert.FileExists(t, filepath.Join(repo.Root, "warmed"))
			if fails {
				assert.Contains(t, log.String(), "module warming failed")
			} else {
				assert.Empty(t, log.String())
			}
		})
	}
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
	for _, checker := range []bool{false, true} {
		t.Run(fmt.Sprintf("checker=%t", checker), func(t *testing.T) {
			for _, recovery := range []bool{false, true} {
				t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
					t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
					repo := testutil.NewTestRepoWithCommit(t)
					var checkout, root string
					var cleanup func()
					var opened *os.File
					wp := &WorkerPool{}
					if checker {
						command := filepath.Join(t.TempDir(), "codex")
						require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
						run, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: repo.HeadSHA()}, command, nil, io.Discard)
						require.NoError(t, err)
						checkout, root, cleanup = run.Repo, filepath.Dir(run.Job.WorkDir), run.cleanup
						t.Cleanup(cleanup)
						for _, parent := range []string{run.Repo, filepath.Join(run.Job.WorkDir, "gocache"), filepath.Join(run.Job.WorkDir, "tmp")} {
							dir := filepath.Join(parent, "readonly")
							require.NoError(t, os.Mkdir(dir, 0o700))
							require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600))
							require.NoError(t, os.Chmod(dir, 0o500))
							t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
						}
					} else {
						var err error
						checkout, cleanup, err = wp.createExactCheckout(t.Context(), "worker-1", &storage.ReviewJob{RepoPath: repo.Root, GitRef: repo.HeadSHA()})
						require.NoError(t, err)
						t.Cleanup(cleanup)
						dir := filepath.Join(checkout, "readonly")
						require.NoError(t, os.Mkdir(dir, 0o500))
						opened, err = os.Open(dir)
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, opened.Close()) })
					}
					outside := t.TempDir()
					require.NoError(t, os.Chmod(outside, 0o500))
					t.Cleanup(func() { _ = os.Chmod(outside, 0o700) })
					require.NoError(t, os.Symlink(outside, filepath.Join(checkout, "external")))
					if recovery {
						require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
					} else {
						cleanup()
					}
					if checker {
						assert.NoDirExists(t, root)
					} else {
						info, err := opened.Stat()
						require.NoError(t, err)
						assert.Equal(t, os.FileMode(0o500), info.Mode().Perm(), "checker-off cleanup changed directory permissions")
					}
					assert.NoDirExists(t, checkout)
					info, err := os.Stat(outside)
					require.NoError(t, err)
					assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
					output, err := exec.CommandContext(t.Context(), "git", "-C", repo.Root, "worktree", "list", "--porcelain").CombinedOutput()
					require.NoError(t, err)
					assert.NotContains(t, normalizeGitListOutput(string(output)), normalizeGitListPath(t, checkout))
				})
			}
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
				require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
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
