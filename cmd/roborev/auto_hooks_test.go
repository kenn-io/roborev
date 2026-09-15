package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestAutoInstallHooks(t *testing.T) {
	t.Parallel()
	for _, location := range []string{"default", "relative git dir", "absolute git dir", "relative worktree", "absolute worktree", "external"} {
		for _, linked := range []bool{false, true} {
			for _, state := range []string{"outdated", "companions", "uninstalled"} {
				t.Run(fmt.Sprintf("%s/linked=%t/%s", location, linked, state), func(t *testing.T) {
					t.Parallel()
					repo := testutil.NewTestRepoWithCommit(t)
					hooksDir := repo.HooksDir
					hooksPath := ""
					inside := true
					switch location {
					case "relative git dir":
						hooksPath = ".git/hooks"
					case "absolute git dir":
						hooksPath = hooksDir
					case "relative worktree", "absolute worktree":
						inside = false
						hooksDir = filepath.Join(repo.Root, ".githooks")
						hooksPath = ".githooks"
						if location == "absolute worktree" {
							hooksPath = hooksDir
						}
					case "external":
						inside = false
						hooksDir = t.TempDir()
						hooksPath = hooksDir
					}
					require.NoError(t, os.MkdirAll(hooksDir, 0o755))
					initial := map[string]string{"pre-push": "#!/bin/sh\nprintf '%s\\n' \"custom hook\"\n"}
					switch state {
					case "outdated":
						for _, name := range []string{"post-commit", "post-rewrite", "pre-push"} {
							initial[name] = "#!/bin/sh\n# roborev " + name + " hook v0\necho custom\n"
						}
					case "companions":
						initial["post-commit"] = githook.GeneratePostCommit()
					}
					for name, content := range initial {
						require.NoError(t, os.WriteFile(filepath.Join(hooksDir, name), []byte(content), 0o755))
					}
					// Disable hooks while committing fixtures, including .git/hooks.
					repo.Run("add", ".")
					repo.Run("-c", "core.hooksPath="+t.TempDir(), "commit", "--allow-empty", "-m", "Add custom hooks")
					root := repo.Root
					if linked {
						root = filepath.Join(t.TempDir(), "wt")
						repo.Run("worktree", "add", "--detach", root)
					}
					if hooksPath != "" {
						repo.Run("config", "core.hooksPath", hooksPath)
					}
					for range 3 {
						autoInstallHooks(t.Context(), root)
					}
					assert := assert.New(t)
					for _, name := range []string{"post-commit", "post-rewrite", "pre-push"} {
						content, err := os.ReadFile(filepath.Join(hooksDir, name))
						if inside && state != "uninstalled" {
							require.NoError(t, err)
							assert.Contains(string(content), githook.VersionMarker(name))
						} else if before, exists := initial[name]; exists {
							require.NoError(t, err)
							assert.Equal(before, string(content), name)
						} else {
							require.ErrorIs(t, err, os.ErrNotExist, name)
						}
					}
					assert.Empty(repo.Run("status", "--porcelain"))
					if linked {
						assert.Empty(repo.Run("-C", root, "status", "--porcelain"))
					}
				})
			}
		}
	}
}

func TestRepairHooksGitDirOnly(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, location := range []string{"default", "relative worktree", "absolute worktree", "external"} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/linked=%t", location, linked), func(t *testing.T) {
				repo := testutil.NewTestRepoWithCommit(t)
				hooksDir := repo.HooksDir
				hooksPath := ""
				switch location {
				case "relative worktree", "absolute worktree":
					hooksDir = filepath.Join(repo.Root, ".githooks")
					hooksPath = ".githooks"
					if location == "absolute worktree" {
						hooksPath = hooksDir
					}
				case "external":
					hooksDir = t.TempDir()
					hooksPath = hooksDir
				}
				require.NoError(t, os.MkdirAll(hooksDir, 0o755))
				stale := githook.GeneratePrePushWithBinary(filepath.Join(t.TempDir(), "old-roborev"))
				hookPath := filepath.Join(hooksDir, "pre-push")
				require.NoError(t, os.WriteFile(hookPath, []byte(stale), 0o755))
				repo.Run("add", ".")
				repo.Run("commit", "--allow-empty", "-m", "Add custom hook")
				root := repo.Root
				if linked {
					root = filepath.Join(t.TempDir(), "wt")
					repo.Run("worktree", "add", "--detach", root)
				}
				if hooksPath != "" {
					repo.Run("config", "core.hooksPath", hooksPath)
				}
				t.Chdir(root)
				cmd := installHookCmd()
				cmd.SetArgs([]string{"repair", "--git-dir-only", "--binary", binary})
				require.NoError(t, cmd.Execute())
				content, err := os.ReadFile(hookPath)
				require.NoError(t, err)
				if location == "default" {
					assert.Contains(t, string(content), fmt.Sprintf("ROBOREV=%q", binary))
				} else {
					assert.Equal(t, stale, string(content))
				}
				assert.Empty(t, repo.Run("status", "--porcelain"))
			})
		}
	}
}

func TestExplicitHookMaintenanceInTrackedDirectory(t *testing.T) {
	for _, action := range []string{"init", "install", "repair"} {
		t.Run(action, func(t *testing.T) {
			testenv.SetDataDir(t)
			repo := testutil.NewTestRepoWithCommit(t)
			stale := "#!/bin/sh\n# roborev pre-push hook v0\nprintf '%s\\n' \"custom hook\"\n"
			repo.CommitFile(".githooks/pre-push", stale, "Add custom hook")
			repo.Run("config", "core.hooksPath", ".githooks")
			t.Chdir(repo.Root)
			binary, err := os.Executable()
			require.NoError(t, err)
			cmd := installHookCmd()
			args := []string{"--binary", binary}
			switch action {
			case "init":
				cmd = initCmd()
				args = append(args, "--no-daemon", "--agent", "test")
			case "repair":
				args = append([]string{"repair"}, args...)
			}
			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute())
			content, err := os.ReadFile(filepath.Join(repo.Root, ".githooks", "pre-push"))
			require.NoError(t, err)
			assert.Contains(t, string(content), githook.PrePushVersionMarker)
			assert.Contains(t, string(content), "printf '%s\\n' \"custom hook\"")
		})
	}
}
