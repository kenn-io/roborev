package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestScheduledAgentGuardPreservesPermissionChecks(t *testing.T) {
	unsafe := agent.AllowUnsafeAgents()
	sandboxDisabled := agent.CodexSandboxDisabled()
	t.Cleanup(func() {
		agent.SetAllowUnsafeAgents(unsafe)
		agent.SetCodexSandboxDisabled(sandboxDisabled)
	})

	job := &storage.ReviewJob{Source: storage.JobSourceScheduled}
	testAgent := agent.NewTestAgent()
	assert.True(t, scheduledAgentAllowed(job, testAgent))

	job.Agentic = true
	assert.False(t, scheduledAgentAllowed(job, testAgent))
	job.Agentic = false
	agent.SetAllowUnsafeAgents(true)
	assert.False(t, scheduledAgentAllowed(job, testAgent))
	agent.SetAllowUnsafeAgents(false)

	codex := agent.NewCodexAgent("codex")
	agent.SetCodexSandboxDisabled(true)
	assert.False(t, scheduledAgentAllowed(job, codex))
	agent.SetCodexSandboxDisabled(false)

	assert.False(t, scheduledAgentAllowed(job, agent.NewOpenCodeAgent("opencode")))
	writableACP := agent.NewACPAgent("acp-agent").WithAgentic(true)
	assert.False(t, scheduledAgentAllowed(job, writableACP))
}
