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
)

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
