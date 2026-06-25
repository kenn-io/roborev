package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestCheckConfiguredAgentRequiresNonEmptyGlobalDefault(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)

	// No global config and no repo agent: not configured.
	assert.Equal(statusMissing, checkConfiguredAgent(repo, true, "codex").Status)

	// Global config present but default_agent empty: still not configured
	// (must match the repo check, which requires a non-empty agent).
	cfgPath := config.GlobalConfigPath()
	require.NoError(t, os.WriteFile(cfgPath, []byte("default_agent = \"\"\n"), 0o600))
	assert.Equal(statusMissing, checkConfiguredAgent(repo, true, "codex").Status)

	// Global config with a non-empty default_agent: configured.
	require.NoError(t, os.WriteFile(cfgPath, []byte("default_agent = \"codex\"\n"), 0o600))
	assert.Equal(statusOK, checkConfiguredAgent(repo, true, "codex").Status)
}

func TestDetectStateUsesGlobalWorkflowReviewAgent(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)

	require.NoError(t, os.WriteFile(
		config.GlobalConfigPath(),
		[]byte("review_agent = \"claude-code\"\n"),
		0o600,
	))

	state := detectState(context.Background(), repo, true)
	configured := quickstartCheckByID(t, state, "configured_agent")
	repoConfig := quickstartCheckByID(t, state, "repo_config")

	assert.Equal(statusOK, configured.Status)
	assert.Contains(configured.Details, "claude-code")
	assert.Equal("roborev init --agent claude-code", repoConfig.FixCommand)
}

func TestCheckConfiguredAgentAcceptsRepoLevelReviewAgent(t *testing.T) {
	repo := newTempGitRepo(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".roborev.toml"),
		[]byte("review_agent_thorough = \"claude-code\"\n"),
		0o644,
	))

	check := checkConfiguredAgent(repo, true, "claude-code")

	assert.Equal(t, statusOK, check.Status)
	assert.Contains(t, check.Details, "claude-code")
}

func TestCheckRepoConfigUsesResolvedRepoConfigPath(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	mainRepo := newTempGitRepo(t)
	configureTempGitIdentity(t, mainRepo)
	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("test\n"), 0o644))
	cmd := exec.Command("git", "add", "README.md")
	cmd.Dir = mainRepo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = mainRepo
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	require.NoError(t, os.WriteFile(
		filepath.Join(mainRepo, ".roborev.toml"),
		[]byte("agent = \"codex\"\n"),
		0o644,
	))

	worktree := filepath.Join(t.TempDir(), "linked")
	cmd = exec.Command("git", "worktree", "add", worktree)
	cmd.Dir = mainRepo
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	check := checkRepoConfig(worktree, true, "codex")

	assert.Equal(t, statusOK, check.Status)
	assert.Contains(t, check.Details, ".roborev.toml present")
}

func TestCheckRepoConfigReportsInvalidConfigAsUnknown(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".roborev.toml"),
		[]byte("agent = \n"),
		0o644,
	))

	check := checkRepoConfig(repo, true, "codex")

	assert.Equal(t, statusUnknown, check.Status)
	assert.Contains(t, check.Details, "could not read .roborev.toml")
}

func TestCheckSkillsRequiresFixAndRefineSkillNamesForOneAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	writeQuickstartSkill(t, home, ".claude", "roborev-address")
	assert.Equal(t, statusMissing, checkSkills().Status)

	writeQuickstartSkill(t, home, ".claude", "roborev-fix")
	assert.Equal(t, statusMissing, checkSkills().Status)

	writeQuickstartSkill(t, home, ".claude", "roborev-refine")
	check := checkSkills()
	assert.Equal(t, statusOK, check.Status)
	assert.Contains(t, check.Details, "Claude Code")
}

func TestCheckSkillsAcceptsCodexFixAndRefine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	writeQuickstartSkill(t, home, ".codex", "roborev-fix")
	writeQuickstartSkill(t, home, ".codex", "roborev-refine")

	check := checkSkills()

	assert.Equal(t, statusOK, check.Status)
	assert.Contains(t, check.Details, "Codex")
}

func writeQuickstartSkill(t *testing.T, home, configDir, name string) {
	t.Helper()
	path := filepath.Join(home, configDir, "skills", name, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("---\nname: "+name+"\n---\n"), 0o644))
}

func quickstartCheckByID(t *testing.T, state quickstartState, id string) quickstartCheck {
	t.Helper()
	var found *quickstartCheck
	for i := range state.Checks {
		if state.Checks[i].ID == id {
			found = &state.Checks[i]
			break
		}
	}
	require.NotNil(t, found, "missing quickstart check %q", id)
	return *found
}

func configureTempGitIdentity(t *testing.T, repo string) {
	t.Helper()
	for _, args := range [][]string{
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.invalid"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
	}
}

func newTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	return dir
}

func TestDetectStateSchema(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)

	state := detectState(context.Background(), repo, true)

	assert.True(state.InGitRepo)

	// Exactly the eight stable IDs, in order.
	var ids []string
	for _, c := range state.Checks {
		ids = append(ids, c.ID)
	}
	assert.Equal(quickstartCheckIDs, ids)

	for _, c := range state.Checks {
		assert.Contains([]checkStatus{statusOK, statusMissing, statusUnknown}, c.Status, c.ID)
		if c.Status == statusMissing {
			assert.NotEmpty(c.FixCommand, "missing check %s must have a fix_command", c.ID)
			assert.NotContains(c.FixCommand, "<agent>", "fix_command must be fully substituted")
		}
	}
}

func TestDetectStateOutsideRepoMarksRepoChecksUnknown(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	state := detectState(context.Background(), "", false)

	assert.False(t, state.InGitRepo)
	repoDependent := map[string]bool{
		"post_commit_hook": true, "repo_registered": true,
		"repo_config": true, "configured_agent": true,
	}
	for _, c := range state.Checks {
		if repoDependent[c.ID] {
			assert.Equal(t, statusUnknown, c.Status, c.ID)
		}
	}
}

func TestDetectStateIsReadOnly(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)
	hookPath := filepath.Join(repo, ".git", "hooks", "post-commit")

	_, errBefore := os.Stat(hookPath)
	require.True(t, os.IsNotExist(errBefore), "precondition: no hook yet")

	_ = detectState(context.Background(), repo, true)

	// Detection must not create a post-commit hook.
	_, errAfter := os.Stat(hookPath)
	assert.True(t, os.IsNotExist(errAfter), "detectState must not create a post-commit hook")
}

func TestStateJSONMarshalsStableFields(t *testing.T) {
	state := quickstartState{
		InGitRepo:     true,
		DaemonRunning: false,
		Checks:        []quickstartCheck{{ID: "daemon_running", Status: statusMissing, FixCommand: "roborev daemon start"}},
	}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Contains(t, back, "in_git_repo")
	assert.Contains(t, back, "daemon_running")
	assert.Contains(t, back, "checks")
}

func TestRenderHumanIncludesGuideAndState(t *testing.T) {
	var buf bytes.Buffer
	renderHuman(&buf, quickstartState{
		InGitRepo: true,
		Checks:    []quickstartCheck{{ID: "daemon_running", Status: statusOK, Details: "daemon is running"}},
	})
	out := buf.String()
	assert.Contains(t, out, "How roborev works") // embedded guide
	assert.Contains(t, out, "daemon_running")    // detected state
}

func TestQuickstartJSONOmitsGuide(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)

	cmd := quickstartCmd()
	cmd.SetArgs([]string{"--json"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	// Run from the repo dir.
	t.Chdir(repo)
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.NotContains(t, out, "How roborev works")
	var state quickstartState
	require.NoError(t, json.Unmarshal([]byte(out), &state))
	assert.Len(t, state.Checks, len(quickstartCheckIDs))
}

func TestQuickstartOutsideGitRepo(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Chdir(t.TempDir())

	t.Run("json exits 0 with in_git_repo false", func(t *testing.T) {
		cmd := quickstartCmd()
		cmd.SetArgs([]string{"--json"})
		var outBuf, errBuf bytes.Buffer
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		require.NoError(t, cmd.Execute())

		var state quickstartState
		require.NoError(t, json.Unmarshal(outBuf.Bytes(), &state))
		assert.False(t, state.InGitRepo)
	})

	t.Run("human returns silentExit error", func(t *testing.T) {
		cmd := quickstartCmd()
		cmd.SetArgs(nil)
		var outBuf, errBuf bytes.Buffer
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		require.Error(t, cmd.Execute())
	})
}
