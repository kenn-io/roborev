package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestProcessJob_TaskAgentPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("synthetic agent uses a POSIX shell")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	for _, tt := range []struct {
		name       string
		agent      string
		jobType    string
		agentic    bool
		loadConfig bool
	}{
		{name: "Codex analysis", agent: "codex", jobType: storage.JobTypeTask},
		{name: "Claude analysis", agent: "claude-code", jobType: storage.JobTypeTask},
		{name: "Codex insights", agent: "codex", jobType: storage.JobTypeInsights},
		{name: "Codex config opt-out", agent: "codex", jobType: storage.JobTypeTask, loadConfig: true},
		{name: "Codex agentic task", agent: "codex", jobType: storage.JobTypeTask, agentic: true},
		{name: "Claude agentic task", agent: "claude-code", jobType: storage.JobTypeTask, agentic: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			commandPath := filepath.Join(t.TempDir(), "agent")
			require.NoError(t, os.WriteFile(commandPath, []byte(`#!/bin/sh
case "$*" in
  *--help*) echo 'usage --sandbox --ignore-user-config --thread-source --dangerously-bypass-approvals-and-sandbox --dangerously-skip-permissions'; exit 0;;
esac
printf '%s\n' "$@" > agent-args.txt
cat > agent-prompt.txt
echo '{"type":"item.completed","item":{"type":"agent_message","text":"Analysis complete."}}'
echo '{"type":"result","result":"Analysis complete."}'
`), 0o755))
			cfg := config.DefaultConfig()
			cfg.CodexCmd = commandPath
			cfg.ClaudeCodeCmd = commandPath
			cfg.Agent.Codex.IgnoreReviewUserConfig = !tt.loadConfig
			cfg.Agent.Codex.Config = map[string]any{"model_provider": "custom-provider"}
			tc.reconfigurePool(cfg)

			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, GitRef: "refactor", Agent: tt.agent,
				Prompt: "Analyze the source files.", JobType: tt.jobType,
				Agentic: tt.agentic,
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)

			tc.Pool.processJob(testWorkerID, claimed)
			tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			captured, err := os.ReadFile(filepath.Join(tc.TmpDir, "agent-args.txt"))
			require.NoError(t, err)
			args := strings.Split(strings.TrimSpace(string(captured)), "\n")
			prompt, err := os.ReadFile(filepath.Join(tc.TmpDir, "agent-prompt.txt"))
			require.NoError(t, err)
			assert.Contains(string(prompt), "Analyze the source files.")

			if tt.agent == "codex" {
				assert.Contains(args, `model_provider="custom-provider"`)
				if tt.agentic {
					assert.Contains(args, "--dangerously-bypass-approvals-and-sandbox")
					assert.NotContains(args, "--ignore-user-config")
				} else {
					assert.Contains(string(captured), "--sandbox\nread-only\n")
					assert.NotContains(args, "--dangerously-bypass-approvals-and-sandbox")
					assert.Equal(!tt.loadConfig, strings.Contains(string(captured), "--ignore-user-config\n"))
				}
			} else if tt.agentic {
				assert.Contains(args, "--dangerously-skip-permissions")
			} else {
				assert.Contains(string(captured), "--tools\nRead,Glob,Grep\n")
				assert.Contains(string(captured), "--disallowedTools\nmcp__*\n")
				assert.NotContains(args, "--dangerously-skip-permissions")
			}
		})
	}
}
