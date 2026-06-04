package agenthook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOutputForStopBlocks(t *testing.T) {
	output := BuildOutput(Input{HookEventName: "Stop"}, Response{
		Triggered: true,
		Reason:    "Invoke $roborev-fix.",
	})

	assert.Equal(t, "block", output["decision"])
	assert.Equal(t, "Invoke $roborev-fix.", output["reason"])
}

func TestBuildOutputForPostToolUseAddsContext(t *testing.T) {
	output := BuildOutput(Input{HookEventName: "PostToolUse"}, Response{
		Triggered: true,
		Reason:    "Invoke $roborev-fix.",
	})

	assert.NotContains(t, output, "decision")
	specific, ok := output["hookSpecificOutput"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "PostToolUse", specific["hookEventName"])
	assert.Equal(t, "Invoke $roborev-fix.", specific["additionalContext"])
}

func TestBuildOutputWhenNotTriggeredIsEmpty(t *testing.T) {
	assert.Empty(t, BuildOutput(Input{HookEventName: "Stop"}, Response{}))
}
