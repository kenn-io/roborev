package agenthook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestRunDumpCodexCreatesHookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run"

	var stdout bytes.Buffer
	err := RunDump(DumpOptions{
		Agent:      "codex",
		Command:    command,
		ConfigPath: path,
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &root))
	assertCommandCount(t, root, "PostToolUse", command, 1)
	assertCommandCount(t, root, "Stop", command, 1)
	assert.Equal(t, "^Bash$", firstMatcher(t, root, "PostToolUse"))
	assert.InDelta(t, 10, firstCommandTimeout(t, root, "Stop", command), 0)
}

func TestRunDumpDroidCreatesHookConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run --agent droid"

	var stdout bytes.Buffer
	err := RunDump(DumpOptions{
		Agent:      "droid",
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &stdout)

	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &root))
	assertCommandCount(t, root, "PreToolUse", command, 1)
	assertCommandCount(t, root, "PostToolUse", command, 1)
	assertCommandCount(t, root, "Stop", command, 1)
	assert.Equal(t, ExecuteMatcher, firstMatcher(t, root, "PreToolUse"))
	assert.Equal(t, ExecuteMatcher, firstMatcher(t, root, "PostToolUse"))
	assert.Empty(t, firstMatcher(t, root, "Stop"))
	assert.InDelta(t, 10, firstCommandTimeout(t, root, "Stop", command), 0)
}

func TestRunInstallCodexIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run"

	var first bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         command,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &first)
	require.NoError(t, err)
	assert.Contains(t, first.String(), "installed Codex agent hooks")

	var second bytes.Buffer
	err = RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         command,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &second)
	require.NoError(t, err)
	assert.Contains(t, second.String(), "Codex agent hooks already installed")
}

func TestRunInstallDroidIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	command := "/tmp/roborev agent-hook run --agent droid"

	var first bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &first)
	require.NoError(t, err)
	assert.Contains(t, first.String(), "installed Factory Droid agent hooks")

	var second bytes.Buffer
	err = RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    command,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &second)
	require.NoError(t, err)
	assert.Contains(t, second.String(), "Factory Droid agent hooks already installed")

	root := readJSONFile(t, path)
	assertCommandCount(t, root, "Stop", command, 1)
}

func TestRunInstallMigratesStaleRoborevHookCommand(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	oldCommand := "/old/versioned/1.2.3/bin/roborev agent-hook run"
	newCommand := "/stable/bin/roborev agent-hook run"

	// A config left by an earlier install carries the old absolute-path command.
	writeJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []any{map[string]any{
				"matcher": "^Bash$",
				"hooks":   []any{commandHookJSON(oldCommand, 10)},
			}},
			"Stop": []any{map[string]any{
				"hooks": []any{commandHookJSON(oldCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         newCommand,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &out)
	require.NoError(t, err)

	// The stale command is replaced in place, not appended beside: each event
	// keeps exactly one command hook, carrying the new path.
	root := readJSONFile(t, path)
	assertCommandCount(t, root, "PostToolUse", newCommand, 1)
	assertCommandCount(t, root, "PostToolUse", oldCommand, 0)
	assertCommandCount(t, root, "Stop", newCommand, 1)
	assertCommandCount(t, root, "Stop", oldCommand, 0)
	assert.Contains(out.String(), "installed Codex agent hooks", "migrating a stale command counts as a change")
}

func TestRunInstallDroidLeavesPlainAgentHookEntriesUntouched(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	agentCommand := "/stable/bin/roborev agent-hook run"
	droidCommand := "/stable/bin/roborev agent-hook run --agent droid"

	writeJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{commandHookJSON(agentCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    droidCommand,
		ConfigPath: path,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.NoError(t, err)

	root := readJSONFile(t, path)
	assertCommandCount(t, root, "Stop", agentCommand, 1)
	assertCommandCount(t, root, "Stop", droidCommand, 1)
	assert.Contains(out.String(), "installed Factory Droid agent hooks")
}

func TestRunInstallCodexLeavesDroidAgentHookEntriesUntouched(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	droidCommand := "/stable/bin/roborev agent-hook run --agent droid"
	codexCommand := "/stable/bin/roborev agent-hook run"

	writeJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{commandHookJSON(droidCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         codexCommand,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &out)
	require.NoError(t, err)

	root := readJSONFile(t, path)
	assertCommandCount(t, root, "Stop", droidCommand, 1)
	assertCommandCount(t, root, "Stop", codexCommand, 1)
	assert.Contains(out.String(), "installed Codex agent hooks")
}

func TestRunInstallCodexMigratesStaleHookCommandWithRunFlags(t *testing.T) {
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	oldCommand := "/old/versioned/1.2.3/bin/roborev agent-hook run --turn-threshold 3"
	newCommand := "/stable/bin/roborev agent-hook run"

	writeJSONFile(t, path, map[string]any{
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{commandHookJSON(oldCommand, 10)},
			}},
		},
	})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:           "codex",
		Command:         newCommand,
		CodexConfigPath: path,
		Timeout:         10 * time.Second,
	}, &out)
	require.NoError(t, err)

	root := readJSONFile(t, path)
	assertCommandCount(t, root, "Stop", newCommand, 1)
	assertCommandCount(t, root, "Stop", oldCommand, 0)
	assert.Contains(out.String(), "installed Codex agent hooks")
}

func TestRunInstallDroidMigratesStaleHookCommandWithRunFlags(t *testing.T) {
	cases := []struct {
		name       string
		oldCommand string
	}{
		{
			name:       "agent flag before config",
			oldCommand: "/old/versioned/1.2.3/bin/roborev agent-hook run --agent droid --config /tmp/roborev.toml",
		},
		{
			name:       "agent flag after config",
			oldCommand: "/old/versioned/1.2.3/bin/roborev agent-hook run --config /tmp/roborev.toml --agent droid",
		},
		{
			name:       "agent equals form",
			oldCommand: "/old/versioned/1.2.3/bin/roborev agent-hook run --agent=droid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			path := filepath.Join(t.TempDir(), "hooks.json")
			newCommand := "/stable/bin/roborev agent-hook run --agent droid"

			writeJSONFile(t, path, map[string]any{
				"hooks": map[string]any{
					"Stop": []any{map[string]any{
						"hooks": []any{commandHookJSON(tc.oldCommand, 10)},
					}},
				},
			})

			var out bytes.Buffer
			err := RunInstall(InstallOptions{
				Agent:      "droid",
				Command:    newCommand,
				ConfigPath: path,
				Scope:      "user",
				Timeout:    10 * time.Second,
			}, &out)
			require.NoError(t, err)

			root := readJSONFile(t, path)
			assertCommandCount(t, root, "Stop", newCommand, 1)
			assertCommandCount(t, root, "Stop", tc.oldCommand, 0)
			assert.Contains(out.String(), "installed Factory Droid agent hooks")
		})
	}
}

func TestRunInstallDroidRejectsUnknownScope(t *testing.T) {
	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:   "droid",
		Command: "/tmp/roborev agent-hook run --agent droid",
		Scope:   "team",
		Timeout: 10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope must be user")
}

func TestRunInstallDroidRejectsProjectScope(t *testing.T) {
	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:   "droid",
		Command: "/tmp/roborev agent-hook run --agent droid",
		Scope:   "project",
		Timeout: 10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project scope is not supported for Factory Droid agent hooks")
}

func TestRunDumpDroidRejectsProjectScope(t *testing.T) {
	var out bytes.Buffer
	err := RunDump(DumpOptions{
		Agent:   "droid",
		Command: "/tmp/roborev agent-hook run --agent droid",
		Scope:   "project",
		Timeout: 10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project scope is not supported for Factory Droid agent hooks")
}

func TestRunInstallDroidRejectsProjectConfigPath(t *testing.T) {
	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(".factory", "hooks.json"),
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunDumpDroidRejectsProjectConfigPath(t *testing.T) {
	var out bytes.Buffer
	err := RunDump(DumpOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(".factory", "hooks.json"),
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunInstallDroidRejectsMixedCaseProjectConfigPathsWhenCaseInsensitive(t *testing.T) {
	stubDroidPathCaseInsensitive(t, true)
	repo := testutil.NewGitRepo(t)
	chdirForTest(t, repo.Path())

	for _, configPath := range []string{
		filepath.Join(".Factory", "hooks.JSON"),
		filepath.Join(repo.Path(), ".Factory", "hooks.JSON"),
	} {
		t.Run(configPath, func(t *testing.T) {
			var out bytes.Buffer
			err := RunInstall(InstallOptions{
				Agent:      "droid",
				Command:    "/tmp/roborev agent-hook run --agent droid",
				ConfigPath: configPath,
				Scope:      "user",
				Timeout:    10 * time.Second,
				DryRun:     true,
			}, &out)
			require.Error(t, err)
			assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
		})
	}
}

func TestRunDumpDroidRejectsMixedCaseProjectConfigPathWhenCaseInsensitive(t *testing.T) {
	stubDroidPathCaseInsensitive(t, true)
	repo := testutil.NewGitRepo(t)
	chdirForTest(t, repo.Path())

	var out bytes.Buffer
	err := RunDump(DumpOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(".Factory", "hooks.JSON"),
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunInstallDroidRejectsMixedCaseExistingProjectConfigOnCaseInsensitiveFS(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	chdirForTest(t, repo.Path())
	requireFilesystemCaseInsensitive(t, repo.Path())
	stubDroidPathCaseInsensitive(t, false)

	require.NoError(t, os.MkdirAll(filepath.Join(repo.Path(), ".factory"), 0o755))
	writeJSONFile(t, filepath.Join(repo.Path(), ".factory", "hooks.json"), map[string]any{})

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(repo.Path(), ".Factory", "hooks.JSON"),
		Scope:      "user",
		Timeout:    10 * time.Second,
		DryRun:     true,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunInstallDroidRejectsMixedCaseNewProjectConfigOnCaseInsensitiveFS(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	chdirForTest(t, repo.Path())
	requireFilesystemCaseInsensitive(t, repo.Path())
	stubDroidPathCaseInsensitive(t, false)

	var out bytes.Buffer
	err := RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(".Factory", "hooks.JSON"),
		Scope:      "user",
		Timeout:    10 * time.Second,
		DryRun:     true,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunInstallDroidRejectsSymlinkToProjectConfigPath(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	targetDir := filepath.Join(repo.Path(), ".factory")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	targetPath := filepath.Join(targetDir, "hooks.json")
	writeJSONFile(t, targetPath, map[string]any{})

	linkPath := filepath.Join(t.TempDir(), "hooks.json")
	err := os.Symlink(targetPath, linkPath)
	if err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out bytes.Buffer
	err = RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: linkPath,
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunInstallDroidAllowsUserScopeWhenHomeIsCWD(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	t.Setenv("HOME", wd)

	var out bytes.Buffer
	err = RunInstall(InstallOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(wd, ".factory", "hooks.json"),
		Scope:      "user",
		Timeout:    10 * time.Second,
		DryRun:     true,
	}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "would update Factory Droid agent hooks")
}

func TestRunDumpDroidAllowsUserScopeWhenHomeIsCWD(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	t.Setenv("HOME", wd)

	var out bytes.Buffer
	err = RunDump(DumpOptions{
		Agent:      "droid",
		Command:    "/tmp/roborev agent-hook run --agent droid",
		ConfigPath: filepath.Join(wd, ".factory", "hooks.json"),
		Scope:      "user",
		Timeout:    10 * time.Second,
	}, &out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "agent-hook run --agent droid")
}

func TestRunInstallDroidRejectsSymlinkedParentToRepoFactory(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo.Path(), ".factory"), 0o755))

	home := t.TempDir()
	t.Setenv("HOME", home)
	err := os.Symlink(filepath.Join(repo.Path(), ".factory"), filepath.Join(home, ".factory"))
	if err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out bytes.Buffer
	err = RunInstall(InstallOptions{
		Agent:   "droid",
		Command: "/tmp/roborev agent-hook run --agent droid",
		Scope:   "user",
		Timeout: 10 * time.Second,
		DryRun:  true,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunDumpDroidRejectsSymlinkedParentToRepoFactory(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo.Path(), ".factory"), 0o755))

	home := t.TempDir()
	t.Setenv("HOME", home)
	err := os.Symlink(filepath.Join(repo.Path(), ".factory"), filepath.Join(home, ".factory"))
	if err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var out bytes.Buffer
	err = RunDump(DumpOptions{
		Agent:   "droid",
		Command: "/tmp/roborev agent-hook run --agent droid",
		Scope:   "user",
		Timeout: 10 * time.Second,
	}, &out)
	require.Error(t, err)
	assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
}

func TestRunInstallDroidRejectsRepoRootProjectConfigFromSubdir(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	subdir := filepath.Join(repo.Path(), "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdirForTest(t, subdir)

	for _, configPath := range []string{
		filepath.Join("..", ".factory", "hooks.json"),
		filepath.Join(repo.Path(), ".factory", "hooks.json"),
	} {
		t.Run(configPath, func(t *testing.T) {
			var out bytes.Buffer
			err := RunInstall(InstallOptions{
				Agent:      "droid",
				Command:    "/tmp/roborev agent-hook run --agent droid",
				ConfigPath: configPath,
				Scope:      "user",
				Timeout:    10 * time.Second,
				DryRun:     true,
			}, &out)
			require.Error(t, err)
			assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
		})
	}
}

func TestRunDumpDroidRejectsRepoRootProjectConfigFromSubdir(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	subdir := filepath.Join(repo.Path(), "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	chdirForTest(t, subdir)

	for _, configPath := range []string{
		filepath.Join("..", ".factory", "hooks.json"),
		filepath.Join(repo.Path(), ".factory", "hooks.json"),
	} {
		t.Run(configPath, func(t *testing.T) {
			var out bytes.Buffer
			err := RunDump(DumpOptions{
				Agent:      "droid",
				Command:    "/tmp/roborev agent-hook run --agent droid",
				ConfigPath: configPath,
				Scope:      "user",
				Timeout:    10 * time.Second,
			}, &out)
			require.Error(t, err)
			assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
		})
	}
}

func TestRunInstallDroidRejectsTargetRepoProjectConfigFromOutsideRepo(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	parent := filepath.Dir(repo.Path())
	chdirForTest(t, parent)

	for _, configPath := range []string{
		filepath.Join(filepath.Base(repo.Path()), ".factory", "hooks.json"),
		filepath.Join(repo.Path(), ".factory", "hooks.json"),
	} {
		t.Run(configPath, func(t *testing.T) {
			var out bytes.Buffer
			err := RunInstall(InstallOptions{
				Agent:      "droid",
				Command:    "/tmp/roborev agent-hook run --agent droid",
				ConfigPath: configPath,
				Scope:      "user",
				Timeout:    10 * time.Second,
				DryRun:     true,
			}, &out)
			require.Error(t, err)
			assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
		})
	}
}

func TestRunDumpDroidRejectsTargetRepoProjectConfigFromOutsideRepo(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	parent := filepath.Dir(repo.Path())
	chdirForTest(t, parent)

	for _, configPath := range []string{
		filepath.Join(filepath.Base(repo.Path()), ".factory", "hooks.json"),
		filepath.Join(repo.Path(), ".factory", "hooks.json"),
	} {
		t.Run(configPath, func(t *testing.T) {
			var out bytes.Buffer
			err := RunDump(DumpOptions{
				Agent:      "droid",
				Command:    "/tmp/roborev agent-hook run --agent droid",
				ConfigPath: configPath,
				Scope:      "user",
				Timeout:    10 * time.Second,
			}, &out)
			require.Error(t, err)
			assert.ErrorContains(t, err, "project-scoped Factory Droid hook config is not supported")
		})
	}
}

func TestDefaultDroidHooksPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	assert.Equal(t, filepath.Join(dir, ".factory", "hooks.json"), DefaultDroidHooksPath("user"))
	assert.Equal(t, filepath.Join(dir, ".factory", "hooks.json"), DefaultDroidHooksPath(""))
	assert.Empty(t, DefaultDroidHooksPath("project"))
}

func TestDefaultClaudeSettingsPathHonorsClaudeConfigDir(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	assert.Equal(t, filepath.Join(configDir, "settings.json"), DefaultClaudeSettingsPath())
}

func TestDefaultClaudeSettingsPathDefaultsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	assert.Equal(t, filepath.Join(home, ".claude", "settings.json"), DefaultClaudeSettingsPath())
}

func TestUpsertCommandHookCollapsesDuplicatesAndKeepsOthers(t *testing.T) {
	assert := assert.New(t)
	spec := InstallSpec{
		Event: "PostToolUse", Matcher: "^Bash$",
		Command: "/new/roborev agent-hook run", Timeout: 10, IncludeTimeout: true,
	}
	commandHook := map[string]any{"type": "command", "command": spec.Command, "timeout": spec.Timeout}
	list := []any{
		commandHookJSON("/old/a/roborev agent-hook run", 10),
		map[string]any{"type": "command", "command": "/usr/bin/other-tool run"},
		commandHookJSON("/old/b/roborev agent-hook run", 10),
	}

	updated, changed := upsertCommandHook(list, commandHook, spec, agentHookRunner)

	assert.True(changed)
	// Both stale roborev hooks collapse into one new command at the first one's
	// slot; the unrelated tool hook is preserved.
	commands := make([]string, 0, len(updated))
	for _, raw := range updated {
		commands = append(commands, raw.(map[string]any)["command"].(string))
	}
	assert.Equal([]string{spec.Command, "/usr/bin/other-tool run"}, commands)
}

func TestAgentHookNoticeTranslatesBinaryFlagForCommandOnlyFlows(t *testing.T) {
	assert := assert.New(t)
	notice := "Warning: roborev appears to be running from a versioned mise install (/x); " +
		"use --binary to install hooks with a stable shim if available"

	got := TranslateBinaryNotice(notice)
	assert.NotContains(got, "--binary", "command-only flows do not expose --binary")
	assert.Contains(got, "--command", "the override flag is translated to --command")
	assert.Empty(TranslateBinaryNotice(""), "an empty notice stays empty")
}

func TestResolveHookCommandOverrideIsVerbatim(t *testing.T) {
	assert := assert.New(t)

	command, notice, err := ResolveHookCommand("/custom/roborev agent-hook run")
	require.NoError(t, err)
	assert.Equal("/custom/roborev agent-hook run", command, "an override is used verbatim")
	assert.Empty(notice, "an override yields no advisory notice")
}

func TestResolveHookCommandWithBinaryUsesBinaryOverride(t *testing.T) {
	assert := assert.New(t)
	binPath := filepath.Join(t.TempDir(), "roborev")
	require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	command, notice, err := ResolveHookCommandWithBinary("", binPath)
	require.NoError(t, err)
	assert.Equal(shellQuote(binPath)+" agent-hook run", command)
	assert.Empty(notice)
}

func TestResolveHookCommandWithBinaryRejectsCommandAndBinary(t *testing.T) {
	_, _, err := ResolveHookCommandWithBinary("/custom/roborev agent-hook run", "/other/roborev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--command and --binary cannot be used together")
}

func TestResolveHookCommandBlankOverrideResolvesBinary(t *testing.T) {
	assert := assert.New(t)

	// A blank override falls back to binary resolution rather than installing an
	// empty command. The resolved path is appended with the run subcommand.
	command, _, err := ResolveHookCommand("   ")
	require.NoError(t, err)
	assert.NotEmpty(command)
	assert.True(strings.HasSuffix(command, " agent-hook run"),
		"resolved command should invoke agent-hook run, got %q", command)
}

func assertCommandCount(t *testing.T, root map[string]any, event, command string, want int) {
	t.Helper()
	count := 0
	for _, hook := range eventEntriesForTest(t, root, event) {
		entry, ok := hook.(map[string]any)
		require.True(t, ok)
		for _, raw := range entry["hooks"].([]any) {
			hookObj, ok := raw.(map[string]any)
			require.True(t, ok)
			if hookObj["type"] == "command" && hookObj["command"] == command {
				count++
			}
		}
	}
	assert.Equal(t, want, count)
}

func firstMatcher(t *testing.T, root map[string]any, event string) string {
	t.Helper()
	entries := eventEntriesForTest(t, root, event)
	require.NotEmpty(t, entries)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	matcher, _ := entry["matcher"].(string)
	return matcher
}

func firstCommandTimeout(t *testing.T, root map[string]any, event, command string) any {
	t.Helper()
	var found any
	for _, hook := range eventEntriesForTest(t, root, event) {
		entry, ok := hook.(map[string]any)
		require.True(t, ok)
		for _, raw := range entry["hooks"].([]any) {
			hookObj, ok := raw.(map[string]any)
			require.True(t, ok)
			if hookObj["type"] == "command" && hookObj["command"] == command {
				found = hookObj["timeout"]
			}
		}
	}
	require.NotNil(t, found, "command hook %q not found for %s", command, event)
	return found
}

func eventEntriesForTest(t *testing.T, root map[string]any, event string) []any {
	t.Helper()
	hooks, ok := root["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooks[event].([]any)
	require.True(t, ok)
	return entries
}

func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	oldWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(oldWD))
	})
}

func stubDroidPathCaseInsensitive(t *testing.T, enabled bool) {
	t.Helper()
	old := droidPathCaseInsensitive
	droidPathCaseInsensitive = enabled
	t.Cleanup(func() {
		droidPathCaseInsensitive = old
	})
}

func requireFilesystemCaseInsensitive(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	require.NoError(t, os.WriteFile(probe, []byte("probe"), 0o600))
	t.Cleanup(func() {
		require.NoError(t, os.Remove(probe))
	})
	if _, err := os.Stat(filepath.Join(dir, "CASE-PROBE")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
}

func commandHookJSON(command string, timeout int) map[string]any {
	return map[string]any{"type": "command", "command": command, "timeout": float64(timeout)}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, body, 0o600))
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(body, &root))
	return root
}
