package agenthook

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostToolUseAdditionalContextPreservesResolvedInstruction(t *testing.T) {
	assert.Equal(
		t,
		"Invoke $roborev-fix.",
		PostToolUseAdditionalContext("Invoke $roborev-fix."),
	)
}

func TestPostToolUseAdditionalContextFallsBackToDefaultInstruction(t *testing.T) {
	assert.Equal(t, DefaultInstruction, PostToolUseAdditionalContext(""))
}

// User policy must remain the final instruction so preceding workflow text
// cannot override or dilute it.
func TestStopReasonWithFixGuidelinesEndsWithPolicy(t *testing.T) {
	got := StopReasonWithFixGuidelines("Resolve reviews.", "Verify before editing.")
	assert.True(t, strings.HasSuffix(got, "Verify before editing."))
	assert.Contains(t, got, "Resolve reviews.")
}

// If an untriggered response gains policy output, passive hook events begin
// interrupting agent sessions.
func TestBuildOutputWithFixGuidelinesKeepsUntriggeredOutputEmpty(t *testing.T) {
	got := BuildOutputWithFixGuidelines(Input{HookEventName: "Stop"}, Response{}, "Verify first.")
	assert.Empty(t, got)
}

// If the empty-policy path stops delegating to the old formatter, existing
// hook registrations can observe a behavior change without opting in.
func TestBuildOutputWithFixGuidelinesPreservesEmptyPolicyOutput(t *testing.T) {
	input := Input{HookEventName: "PostToolUse"}
	resp := Response{Triggered: true, Reason: "Resolve reviews."}
	assert.Equal(t, BuildOutput(input, resp), BuildOutputWithFixGuidelines(input, resp, ""))
}

func TestBuildOutputInjectsAdditionalContextForScheduledZCodeEvents(t *testing.T) {
	assert := assert.New(t)
	resp := Response{Triggered: true, Reason: "[roborev] demo has findings."}

	for _, event := range []string{"UserPromptSubmit", "SessionStart"} {
		output := BuildOutput(Input{HookEventName: event}, resp)
		specific, ok := output["hookSpecificOutput"].(map[string]any)
		require.True(t, ok)
		assert.Equal(event, specific["hookEventName"])
		assert.Contains(specific["additionalContext"], "[roborev] demo has findings.")
		_, blocked := output["decision"]
		assert.False(blocked)
	}

	// Lifecycle events keep the stop-decision form.
	stop := BuildOutput(Input{HookEventName: "Stop"}, resp)
	assert.Equal("block", stop["decision"])
}
