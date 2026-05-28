//go:build integration && acp

package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acpIntegrationEnableEnv      = "ROBOREV_RUN_ACP_INTEGRATION"
	acpIntegrationCommandEnv     = "ROBOREV_ACP_TEST_COMMAND"
	acpIntegrationArgsEnv        = "ROBOREV_ACP_TEST_ARGS"
	acpIntegrationDisableModeEnv = "ROBOREV_ACP_TEST_DISABLE_MODE"
	acpIntegrationModeEnv        = "ROBOREV_ACP_TEST_MODE"
	acpIntegrationModelEnv       = "ROBOREV_ACP_TEST_MODEL"
)

// TestACPReviewViaExternalAdapter exercises ACP end-to-end against a real wrapper command.
//
// Example:
//
//	make test-acp-integration
//	make test-acp-integration-codex
//	make test-acp-integration-claude
//	make test-acp-integration-gemini
//
// Override command if needed:
//
//	make test-acp-integration ACP_TEST_COMMAND=codex-acp
//	make test-acp-integration ACP_TEST_DISABLE_MODE=1
func TestACPReviewViaExternalAdapter(t *testing.T) {
	if os.Getenv(acpIntegrationEnableEnv) != "1" {
		t.Skipf("set %s=1 to run ACP integration tests", acpIntegrationEnableEnv)
	}

	command := strings.TrimSpace(os.Getenv(acpIntegrationCommandEnv))
	if command == "" {
		command = defaultACPCommand
	}
	args := strings.Fields(strings.TrimSpace(os.Getenv(acpIntegrationArgsEnv)))

	_, err := exec.LookPath(command)
	require.NoError(t, err, "ACP command %q not found in PATH: %v", command, err)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for ACP integration smoke test")
	}

	repoPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("# ACP Integration Test\n"), 0o644); err != nil {
		require.NoError(t, err, "failed to write test file: %v")
	}

	gitRun := func(arguments ...string) string {
		t.Helper()
		cmd := exec.Command("git", arguments...)
		cmd.Dir = repoPath
		out, err := cmd.CombinedOutput()
		assert.Equal(t, false, err != nil)
		return string(out)
	}

	gitRun("init")
	disabledHooksDir := filepath.Join(repoPath, ".git", "hooks-disabled")
	if err := os.MkdirAll(disabledHooksDir, 0o755); err != nil {
		require.NoError(t, err, "failed to create disabled hooks dir: %v")
	}
	gitRun("config", "user.email", "acp-integration@example.com")
	gitRun("config", "user.name", "ACP Integration")
	gitRun("add", "README.md")
	gitRun("-c", "commit.gpgsign=false", "-c", "core.hooksPath="+disabledHooksDir, "commit", "--no-verify", "-m", "seed")
	commitSHA := strings.TrimSpace(gitRun("rev-parse", "HEAD"))
	assert.Equal(t, false, commitSHA == "")

	agent := NewACPAgent(command)
	agent.Args = args
	agent.Timeout = 2 * time.Minute
	if strings.EqualFold(strings.TrimSpace(os.Getenv(acpIntegrationDisableModeEnv)), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv(acpIntegrationDisableModeEnv)), "true") {
		agent.Mode = ""
	}
	if mode := strings.TrimSpace(os.Getenv(acpIntegrationModeEnv)); mode != "" {
		agent.Mode = mode
	}
	if model := strings.TrimSpace(os.Getenv(acpIntegrationModelEnv)); model != "" {
		agent.Model = model
	}

	var streamOutput bytes.Buffer
	result, err := agent.Review(
		context.Background(),
		repoPath,
		commitSHA,
		"Connectivity smoke test: respond with exactly 'ACP_OK' after reading README.md. Do not run roborev commands.",
		&streamOutput,
	)
	assert.Equal(t, false, err != nil)

	trimmed := strings.TrimSpace(result)
	assert.Equal(t, false, trimmed == "")
	assert.Equal(t, false, !strings.Contains(trimmed, "ACP_OK"))
}
