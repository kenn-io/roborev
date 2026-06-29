package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func assertError(t *testing.T, err error, expectError bool, contains string) {
	t.Helper()
	if expectError {
		require.Error(t, err, "expected error but got nil")
		assert.Condition(t, func() bool { return contains == "" || strings.Contains(err.Error(), contains) }, "error text")
	} else {
		require.NoError(t, err)
	}
}

func TestUninstallHookCmd(t *testing.T) {
	tests := []struct {
		name         string
		initialHooks map[string]string
		setup        func(t *testing.T, repo *testutil.TestRepo)
		assert       func(t *testing.T, repo *testutil.TestRepo)
	}{
		{
			name: "hook missing",
			assert: func(t *testing.T, repo *testutil.TestRepo) {
				_, err := os.Stat(repo.HookPath)
				require.ErrorIs(t, err, fs.ErrNotExist)
			},
		},
		{
			name: "hook without roborev",
			initialHooks: map[string]string{
				"post-commit": "#!/bin/bash\necho 'other hook'\n",
			},
			assert: func(t *testing.T, repo *testutil.TestRepo) {
				content, err := os.ReadFile(repo.HookPath)
				require.NoError(t, err, "Failed to read hook: %v")

				want := "#!/bin/bash\necho 'other hook'\n"
				assert.Equal(t, want, string(content))
			},
		},
		{
			name: "hook with roborev only - removes file",
			initialHooks: map[string]string{
				"post-commit": githook.GeneratePostCommit(),
			},
			assert: func(t *testing.T, repo *testutil.TestRepo) {
				_, err := os.Stat(repo.HookPath)
				require.ErrorIs(t, err, fs.ErrNotExist)
			},
		},
		{
			name: "hook with roborev and other commands - preserves others",
			initialHooks: map[string]string{
				"post-commit": "#!/bin/sh\necho 'before'\necho 'after'\n" + githook.GeneratePostCommit(),
			},
			assert: func(t *testing.T, repo *testutil.TestRepo) {
				content, err := os.ReadFile(repo.HookPath)
				require.NoError(t, err, "Failed to read hook: %v")

				contentStr := string(content)
				assert.NotContains(t, contentStr, githook.PostCommitVersionMarker)
				assert.NotContains(t, contentStr, `"$ROBOREV" post-commit`)
				assert.Contains(t, contentStr, "echo 'before'")
				assert.Contains(t, contentStr, "echo 'after'")
			},
		},
		{
			name: "also removes post-rewrite hook",
			initialHooks: map[string]string{
				"post-commit":  githook.GeneratePostCommit(),
				"post-rewrite": githook.GeneratePostRewrite(),
			},
			assert: func(t *testing.T, repo *testutil.TestRepo) {
				_, err := os.Stat(repo.HookPath)
				require.ErrorIs(t, err, fs.ErrNotExist)
				prPath := filepath.Join(repo.HooksDir, "post-rewrite")
				_, err = os.Stat(prPath)
				require.ErrorIs(t, err, fs.ErrNotExist)
			},
		},
		{
			name: "removes post-rewrite even without post-commit",
			initialHooks: map[string]string{
				"post-rewrite": githook.GeneratePostRewrite(),
			},
			assert: func(t *testing.T, repo *testutil.TestRepo) {
				prPath := filepath.Join(repo.HooksDir, "post-rewrite")
				_, err := os.Stat(prPath)
				require.ErrorIs(t, err, fs.ErrNotExist)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testutil.NewTestRepo(t)
			t.Cleanup(repo.Chdir())

			if tt.setup != nil {
				tt.setup(t, repo)
			}

			if len(tt.initialHooks) > 0 {
				err := os.MkdirAll(repo.HooksDir, 0o755)
				require.NoError(t, err)
				for name, content := range tt.initialHooks {
					path := filepath.Join(repo.HooksDir, name)
					err = os.WriteFile(path, []byte(content), 0o755)
					require.NoError(t, err)
				}
			}

			cmd := uninstallHookCmd()
			err := cmd.Execute()
			require.NoError(t, err, "uninstall-hook failed: %v")

			tt.assert(t, repo)
		})
	}
}

func TestInstallHookCmdCreatesHooksDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	repo := testutil.NewTestRepo(t)
	repo.RemoveHooksDir()

	_, err := os.Stat(repo.HooksDir)
	require.ErrorIs(t, err, os.ErrNotExist, "hooks directory should not exist before test")

	t.Cleanup(repo.Chdir())

	installCmd := installHookCmd()
	installCmd.SetArgs([]string{})
	err = installCmd.Execute()

	require.NoError(t, err, "install-hook command failed: %v")

	_, err = os.Stat(repo.HooksDir)
	require.NoError(t, err)

	_, err = os.Stat(repo.HookPath)
	require.NoError(t, err)
}

func TestInstallHookCmdCreatesPostRewriteHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	repo := testutil.NewTestRepo(t)
	repo.RemoveHooksDir()
	t.Cleanup(repo.Chdir())

	installCmd := installHookCmd()
	installCmd.SetArgs([]string{})
	err := installCmd.Execute()
	require.NoError(t, err, "install-hook failed: %v")

	prHookPath := filepath.Join(repo.HooksDir, "post-rewrite")
	content, err := os.ReadFile(prHookPath)
	require.NoError(t, err, "post-rewrite hook not created: %v")

	assert.Contains(t, string(content), "remap --quiet")
	assert.Contains(t, string(content), githook.PostRewriteVersionMarker)
}

func TestInstallHookRepairCmdRewritesCurrentManagedHooksOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	oldBinary := filepath.Join(t.TempDir(), "roborev-old")
	newBinary := filepath.Join(t.TempDir(), "roborev-new")
	require.NoError(t, os.WriteFile(oldBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	repo := testutil.NewTestRepo(t)
	t.Cleanup(repo.Chdir())

	require.NoError(t, os.MkdirAll(repo.HooksDir, 0o755))
	require.NoError(t, os.WriteFile(repo.HookPath, []byte(githook.GeneratePostCommitWithBinary(oldBinary)), 0o755))
	customPostRewrite := "#!/bin/sh\necho custom\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo.HooksDir, "post-rewrite"), []byte(customPostRewrite), 0o755))

	cmd := installHookCmd()
	cmd.SetArgs([]string{"repair", "--binary", newBinary})
	err := cmd.Execute()
	require.NoError(t, err)

	postCommit, err := os.ReadFile(repo.HookPath)
	require.NoError(t, err)
	assert.Contains(t, string(postCommit), fmt.Sprintf("ROBOREV=%q", newBinary))
	assert.NotContains(t, string(postCommit), oldBinary)

	postRewrite, err := os.ReadFile(filepath.Join(repo.HooksDir, "post-rewrite"))
	require.NoError(t, err)
	assert.Equal(t, customPostRewrite, string(postRewrite))
}

func TestInstallHookRepairCmdSkipsCurrentRepoWithoutManagedHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	newBinary := filepath.Join(t.TempDir(), "roborev-new")
	require.NoError(t, os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	repo := testutil.NewTestRepo(t)
	t.Cleanup(repo.Chdir())
	require.NoError(t, os.MkdirAll(repo.HooksDir, 0o755))

	customPostCommit := "#!/bin/sh\necho custom\n"
	require.NoError(t, os.WriteFile(repo.HookPath, []byte(customPostCommit), 0o755))

	cmd := installHookCmd()
	cmd.SetArgs([]string{"repair", "--binary", newBinary})
	err := cmd.Execute()
	require.NoError(t, err)

	postCommit, err := os.ReadFile(repo.HookPath)
	require.NoError(t, err)
	assert.Equal(t, customPostCommit, string(postCommit))

	_, err = os.Stat(filepath.Join(repo.HooksDir, "post-rewrite"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestInstallHookRepairCmdDoesNotTreatRoborevMentionAsManagedHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	newBinary := filepath.Join(t.TempDir(), "roborev-new")
	require.NoError(t, os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	repo := testutil.NewTestRepo(t)
	t.Cleanup(repo.Chdir())
	require.NoError(t, os.MkdirAll(repo.HooksDir, 0o755))

	customPostCommit := "#!/bin/sh\n# roborev is handled elsewhere\necho custom\n"
	require.NoError(t, os.WriteFile(repo.HookPath, []byte(customPostCommit), 0o755))

	cmd := installHookCmd()
	cmd.SetArgs([]string{"repair", "--binary", newBinary})
	err := cmd.Execute()
	require.NoError(t, err)

	postCommit, err := os.ReadFile(repo.HookPath)
	require.NoError(t, err)
	assert.Equal(t, customPostCommit, string(postCommit))
}

func TestInstallHookRepairCmdDoesNotNormalizeUnmanagedHooksPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	newBinary := filepath.Join(t.TempDir(), "roborev-new")
	require.NoError(t, os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	repo := testutil.NewTestRepo(t)
	t.Cleanup(repo.Chdir())

	const relativeHooksPath = "relative-hooks"
	repo.Run("config", "core.hooksPath", relativeHooksPath)
	hooksDir := filepath.Join(repo.Root, relativeHooksPath)
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	customPostCommit := "#!/bin/sh\necho custom\n"
	require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "post-commit"), []byte(customPostCommit), 0o755))

	cmd := installHookCmd()
	cmd.SetArgs([]string{"repair", "--binary", newBinary})
	err := cmd.Execute()
	require.NoError(t, err)

	gotHooksPath := repo.Run("config", "--get", "core.hooksPath")
	assert.Equal(t, relativeHooksPath, gotHooksPath)

	postCommit, err := os.ReadFile(filepath.Join(hooksDir, "post-commit"))
	require.NoError(t, err)
	assert.Equal(t, customPostCommit, string(postCommit))
}

func TestInstallHookRepairCmdWithRegisteredRepairsRegisteredRepos(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	dataDir := testenv.SetDataDir(t)
	db, err := storage.Open(filepath.Join(dataDir, "reviews.db"))
	require.NoError(t, err)
	defer db.Close()

	oldBinary := filepath.Join(t.TempDir(), "roborev-old")
	newBinary := filepath.Join(t.TempDir(), "roborev-new")
	require.NoError(t, os.WriteFile(oldBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	repo := testutil.NewTestRepo(t)
	_, err = db.GetOrCreateRepo(repo.Root)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(repo.HooksDir, 0o755))
	require.NoError(t, os.WriteFile(repo.HookPath, []byte(githook.GeneratePostCommitWithBinary(oldBinary)), 0o755))

	cmd := installHookCmd()
	cmd.SetArgs([]string{"repair", "--registered", "--binary", newBinary})
	err = cmd.Execute()
	require.NoError(t, err)

	postCommit, err := os.ReadFile(repo.HookPath)
	require.NoError(t, err)
	assert.Contains(t, string(postCommit), fmt.Sprintf("ROBOREV=%q", newBinary))
	assert.NotContains(t, string(postCommit), oldBinary)
}

func TestInstallHookRepairCmdWithRegisteredWorksOutsideGitRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test checks Unix exec bits, skipping on Windows")
	}

	dataDir := testenv.SetDataDir(t)
	db, err := storage.Open(filepath.Join(dataDir, "reviews.db"))
	require.NoError(t, err)
	defer db.Close()

	oldBinary := filepath.Join(t.TempDir(), "roborev-old")
	newBinary := filepath.Join(t.TempDir(), "roborev-new")
	require.NoError(t, os.WriteFile(oldBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	repo := testutil.NewTestRepo(t)
	_, err = db.GetOrCreateRepo(repo.Root)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(repo.HooksDir, 0o755))
	require.NoError(t, os.WriteFile(repo.HookPath, []byte(githook.GeneratePostCommitWithBinary(oldBinary)), 0o755))

	outsideRepo := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(outsideRepo))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(previousDir))
	})

	cmd := installHookCmd()
	cmd.SetArgs([]string{"repair", "--registered", "--binary", newBinary})
	err = cmd.Execute()
	require.NoError(t, err)

	postCommit, err := os.ReadFile(repo.HookPath)
	require.NoError(t, err)
	assert.Contains(t, string(postCommit), fmt.Sprintf("ROBOREV=%q", newBinary))
	assert.NotContains(t, string(postCommit), oldBinary)
}

func TestIsTransportError(t *testing.T) {
	t.Run("url.Error wrapping OpError is transport error", func(t *testing.T) {
		err := &url.Error{Op: "Post", URL: "http://127.0.0.1:7373", Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: errors.New("connection refused"),
		}}
		assert.True(t, isTransportError(err))
	})

	t.Run("url.Error without OpError is not transport error", func(t *testing.T) {
		err := &url.Error{Op: "Post", URL: "http://127.0.0.1:7373", Err: errors.New("some non-transport error")}
		assert.False(t, isTransportError(err))
	})

	t.Run("registerRepoError is not transport error", func(t *testing.T) {
		err := &registerRepoError{StatusCode: 500, Body: "internal error"}
		assert.False(t, isTransportError(err))
	})

	t.Run("plain error is not transport error", func(t *testing.T) {
		err := fmt.Errorf("something else")
		assert.False(t, isTransportError(err))
	})

	t.Run("wrapped url.Error with OpError is transport error", func(t *testing.T) {
		inner := &url.Error{Op: "Post", URL: "http://127.0.0.1:7373", Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: errors.New("connection refused"),
		}}
		err := fmt.Errorf("register failed: %w", inner)
		assert.True(t, isTransportError(err))
	})
}

func TestRegisterRepoError(t *testing.T) {
	err := &registerRepoError{StatusCode: 500, Body: "internal server error"}
	assert.Equal(t, "server returned 500: internal server error", err.Error())

	var regErr *registerRepoError
	require.ErrorAs(t, err, &regErr)
	assert.Equal(t, 500, regErr.StatusCode)
}

// initNoDaemonSetup prepares the environment for init --no-daemon tests:
// isolated HOME, fake roborev binary, and chdir to a test repo.
func initNoDaemonSetup(t *testing.T) *testutil.TestRepo {
	t.Helper()

	tmpHome := t.TempDir()
	tmpXDGConfig := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	t.Setenv("XDG_CONFIG_HOME", tmpXDGConfig)
	t.Setenv("ROBOREV_DATA_DIR", filepath.Join(tmpHome, ".roborev"))

	repo := testutil.NewTestRepo(t)
	t.Cleanup(testutil.MockBinaryInPath(t, "roborev", "#!/bin/sh\nexit 0\n"))
	t.Cleanup(repo.Chdir())

	return repo
}

func setupMockServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	oldAddr := serverAddr
	serverAddr = ts.URL
	t.Cleanup(func() { serverAddr = oldAddr })
}

func TestInitNoDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	tests := []struct {
		name           string
		serverHandler  http.HandlerFunc
		setupFiles     func(t *testing.T, repo *testutil.TestRepo)
		expectContains []string
		expectNot      []string
		expectError    bool
		errorContains  string
		postCheck      func(t *testing.T, repo *testutil.TestRepo)
	}{
		{
			name:          "Connection Error",
			serverHandler: nil, // Simulates bad connection
			expectContains: []string{
				"Daemon not running",
				"Setup incomplete",
			},
			expectNot: []string{"Ready!"},
		},
		{
			name: "Server Error 500",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
				_, _ = w.Write([]byte("database locked"))
			},
			expectContains: []string{
				"Warning: failed to register repo",
				"500",
				"Setup incomplete",
			},
			expectNot: []string{"Ready!"},
		},
		{
			name: "Success",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			},
			expectContains: []string{
				"Repo registered with running daemon",
				"Ready!",
			},
			expectNot: []string{"Setup incomplete"},
		},
		{
			name: "Installs PostRewrite Hook On Upgrade",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			},
			setupFiles: func(t *testing.T, repo *testutil.TestRepo) {
				err := os.MkdirAll(repo.HooksDir, 0o755)
				require.NoError(t, err)
				err = os.WriteFile(filepath.Join(repo.HooksDir, "post-commit"), []byte(githook.GeneratePostCommit()), 0o755)
				require.NoError(t, err)
			},
			postCheck: func(t *testing.T, repo *testutil.TestRepo) {
				prHookPath := filepath.Join(repo.HooksDir, "post-rewrite")
				content, err := os.ReadFile(prHookPath)
				require.NoError(t, err, "post-rewrite hook should be installed: %v")

				assert.Contains(t, string(content), "remap --quiet")
			},
		},
		{
			name: "Warns On Non-Shell Hook",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			},
			setupFiles: func(t *testing.T, repo *testutil.TestRepo) {
				os.MkdirAll(repo.HooksDir, 0o755)
				os.WriteFile(
					filepath.Join(repo.HooksDir, "post-commit"),
					[]byte("#!/usr/bin/env python3\nprint('hello')\n"),
					0o755,
				)
			},
			expectContains: []string{
				"non-shell interpreter",
				"Ready!",
			},
		},
		{
			name: "Fails On Hook Write Error",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			},
			setupFiles: func(t *testing.T, repo *testutil.TestRepo) {
				os.MkdirAll(repo.HooksDir, 0o755)
				os.Chmod(repo.HooksDir, 0o555)
				t.Cleanup(func() { os.Chmod(repo.HooksDir, 0o755) })
			},
			expectError:   true,
			errorContains: "install hooks",
		},
		{
			name: "Fails On Mixed Hook Errors",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			},
			setupFiles: func(t *testing.T, repo *testutil.TestRepo) {
				os.MkdirAll(repo.HooksDir, 0o755)
				os.WriteFile(
					filepath.Join(repo.HooksDir, "post-commit"),
					[]byte("#!/usr/bin/env python3\nprint('hello')\n"),
					0o755,
				)
				// Create post-rewrite as a directory so writing it fails.
				os.MkdirAll(filepath.Join(repo.HooksDir, "post-rewrite"), 0o755)
			},
			expectError:   true,
			errorContains: "install hooks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := initNoDaemonSetup(t)

			if tt.setupFiles != nil {
				tt.setupFiles(t, repo)
			}

			if tt.serverHandler != nil {
				setupMockServer(t, tt.serverHandler)
			} else {
				// Simulate connection error
				oldAddr := serverAddr
				serverAddr = "http://127.0.0.1:1"
				t.Cleanup(func() { serverAddr = oldAddr })
			}

			output := captureStdout(t, func() {
				cmd := initCmd()
				cmd.SetArgs([]string{"--no-daemon"})
				err := cmd.Execute()
				assertError(t, err, tt.expectError, tt.errorContains)
			})

			for _, s := range tt.expectContains {
				assert.Contains(t, output, s)
			}
			for _, s := range tt.expectNot {
				assert.NotContains(t, output, s)
			}

			if tt.postCheck != nil {
				tt.postCheck(t, repo)
			}
		})
	}
}

func TestInitNoDaemonWithAgentCreatesCommentedRepoConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	repo := initNoDaemonSetup(t)
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	output := captureStdout(t, func() {
		cmd := initCmd()
		cmd.SetArgs([]string{"--no-daemon", "--agent", "codex"})
		if err := cmd.Execute(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "init failed: %v", err)
		}
	})

	if !strings.Contains(output, "Created ") || !strings.Contains(output, ".roborev.toml") {
		require.Condition(t, func() bool {
			return false
		}, "init output missing repo config creation message:\n%s", output)
	}

	data, err := os.ReadFile(filepath.Join(repo.Root, ".roborev.toml"))
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "read repo config: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"# Default agent for this repo when no workflow-specific agent is set.\n",
		"agent = 'codex'",
	} {
		if !strings.Contains(got, want) {
			require.Condition(t, func() bool {
				return false
			}, "repo config missing %q:\n%s", want, got)
		}
	}
}

func TestInitNoDaemonEnsuresDefaultSnapshotDirGitignored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	repo := initNoDaemonSetup(t)
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon"})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(filepath.Join(repo.Root, ".gitignore"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "# roborev snapshots\n/.roborev/\n")

	check := exec.Command("git", "-C", repo.Root, "check-ignore", "--quiet", "--no-index", ".roborev/roborev-snapshot-x/file.diff")
	require.NoError(t, check.Run())
}

func TestInitNoDaemonWithBinaryInstallsHooksUsingSpecifiedBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	repo := initNoDaemonSetup(t)
	binPath := filepath.Join(t.TempDir(), "roborev")
	require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon", "--binary", binPath})
	require.NoError(t, cmd.Execute())

	for _, hookName := range []string{"post-commit", "post-rewrite"} {
		content, err := os.ReadFile(filepath.Join(repo.HooksDir, hookName))
		require.NoError(t, err)
		assert.Contains(t, string(content), fmt.Sprintf("ROBOREV=%q", binPath))
	}
}

func TestInitNoDaemonEnsuresConfiguredSnapshotDirGitignored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	repo := initNoDaemonSetup(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo.Root, ".roborev.toml"),
		[]byte("snapshot_dir = \"var/roborev\"\n"),
		0o644,
	))
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon"})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(filepath.Join(repo.Root, ".gitignore"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "# roborev snapshots\n/var/roborev/\n")

	check := exec.Command("git", "-C", repo.Root, "check-ignore", "--quiet", "--no-index", "var/roborev/roborev-snapshot-x/file.diff")
	require.NoError(t, check.Run())
}

func TestInitNoDaemonLeavesExistingSnapshotIgnoreRuleAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	repo := initNoDaemonSetup(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo.Root, ".gitignore"),
		[]byte(".roborev/\n"),
		0o644,
	))
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon"})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(filepath.Join(repo.Root, ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, ".roborev/\n", string(data))
}

func TestInitNoDaemonRefusesTrackedSnapshotDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to shell script stubs")
	}

	repo := initNoDaemonSetup(t)
	repo.Config("user.email", testutil.GitUserEmail)
	repo.Config("user.name", testutil.GitUserName)
	repo.WriteFile(".roborev/tracked.txt", "tracked\n")
	repo.RunGit("add", "-f", ".roborev/tracked.txt")
	repo.RunGit("commit", "-m", "track roborev dir")
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon"})
	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracked files")
}

func TestInitNoDaemonRefusesSnapshotDirSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to symlink semantics")
	}

	repo := initNoDaemonSetup(t)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(repo.Root, ".roborev")))
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon"})
	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlinks")
}

func TestInitNoDaemonRefusesGitignoreSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows due to symlink semantics")
	}

	repo := initNoDaemonSetup(t)
	target := filepath.Join(repo.Root, "target-ignore")
	require.NoError(t, os.WriteFile(target, []byte(""), 0o644))
	require.NoError(t, os.Symlink(target, filepath.Join(repo.Root, ".gitignore")))
	setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	cmd := initCmd()
	cmd.SetArgs([]string{"--no-daemon"})
	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), ".gitignore")
	assert.Contains(t, err.Error(), "symlink")
}

func TestInstallHookFromLinkedWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix worktree semantics")
	}

	// Set up a main repo with a relative core.hooksPath but
	// no hooks installed yet.
	repo := testutil.NewTestRepoWithCommit(t)
	customHooks := filepath.Join(repo.Root, ".githooks")
	require.NoError(t, os.MkdirAll(customHooks, 0o755))

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(
			os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}

	runGit(repo.Root, "config", "core.hooksPath", ".githooks")

	// Create a linked worktree.
	wtDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(wtDir)
	require.NoError(t, err)
	runGit(repo.Root, "worktree", "add", resolved, "-b", "wt")

	// Run install-hook from the worktree.
	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(resolved))
	t.Cleanup(func() { os.Chdir(origDir) })

	installCmd := installHookCmd()
	installCmd.SetArgs([]string{})
	require.NoError(t, installCmd.Execute())

	// The hooks should be installed in the main repo's .githooks.
	for _, name := range []string{"post-commit", "post-rewrite"} {
		_, err := os.Stat(filepath.Join(customHooks, name))
		assert.NoError(t, err,
			"%s should exist in shared hooks dir", name)
	}
}

func TestUninstallHookFromLinkedWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix worktree semantics")
	}

	// Set up a main repo with a relative core.hooksPath and
	// installed hooks.
	repo := testutil.NewTestRepoWithCommit(t)
	customHooks := filepath.Join(repo.Root, ".githooks")
	require.NoError(t, os.MkdirAll(customHooks, 0o755))

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(
			os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}

	// Set relative core.hooksPath, install hooks.
	runGit(repo.Root, "config", "core.hooksPath", ".githooks")
	for _, name := range []string{"post-commit", "post-rewrite"} {
		var content string
		if name == "post-commit" {
			content = githook.GeneratePostCommit()
		} else {
			content = githook.GeneratePostRewrite()
		}
		require.NoError(t, os.WriteFile(
			filepath.Join(customHooks, name),
			[]byte(content), 0o755,
		))
	}

	// Create a linked worktree.
	wtDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(wtDir)
	require.NoError(t, err)
	runGit(repo.Root, "worktree", "add", resolved, "-b", "wt")

	// Run uninstall-hook from the worktree.
	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(resolved))
	t.Cleanup(func() { os.Chdir(origDir) })

	cmd := uninstallHookCmd()
	require.NoError(t, cmd.Execute())

	// The hooks in the main repo's .githooks should be removed.
	for _, name := range []string{"post-commit", "post-rewrite"} {
		_, err := os.Stat(filepath.Join(customHooks, name))
		assert.ErrorIs(t, err, fs.ErrNotExist,
			"%s should be removed from shared hooks dir", name)
	}
}
