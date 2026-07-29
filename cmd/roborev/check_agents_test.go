package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestCheckAgentsHonorsConfiguredCodexCommand(t *testing.T) {
	setupConfigEnv(t, "codex_cmd = 'codex-proxy'\n", "")
	t.Cleanup(testutil.MockExecutable(t, "codex", 1))
	t.Cleanup(testutil.MockExecutable(t, "codex-proxy", 1))

	cmd := checkAgentsCmd()
	cmd.SetArgs([]string{"--agent", "codex", "--timeout", "1"})
	output := captureOutput(t, cmd.Execute)

	assert.Regexp(t, `(?m)^  \? codex\s+codex-proxy \(`, output)
}

func TestCheckAgentsDiscoversConfiguredACPAgent(t *testing.T) {
	setupConfigEnv(t, "[acp.goose]\ncommand = 'missing-goose-acp'\n", "")

	cmd := checkAgentsCmd()
	cmd.SetArgs([]string{"--agent", "acp.goose", "--timeout", "1"})
	output := captureOutput(t, cmd.Execute)

	assert.Regexp(t, `(?m)^  - acp\.goose\s+agent "acp\.goose" command "missing-goose-acp" unavailable:`, output)
	assert.Contains(t, output, "1 skipped")
}

func TestCheckAgentsDiscoversRepoACPAgentFromNestedDirectory(t *testing.T) {
	env := setupConfigEnv(t, "", "[acp.goose]\ncommand = 'missing-nested-goose-acp'\n")
	nested := filepath.Join(env.RepoDir, "nested", "directory")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	t.Chdir(nested)

	cmd := checkAgentsCmd()
	cmd.SetArgs([]string{"--agent", "acp.goose", "--timeout", "1"})
	output := captureOutput(t, cmd.Execute)

	assert.Regexp(t, `(?m)^  - acp\.goose\s+agent "acp\.goose" command "missing-nested-goose-acp" unavailable:`, output)
	assert.Contains(t, output, "1 skipped")
}
