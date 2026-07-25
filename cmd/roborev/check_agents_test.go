package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
