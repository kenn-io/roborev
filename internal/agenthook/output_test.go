package agenthook

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPostToolUseAdditionalContextContinuesInterruptedTask(t *testing.T) {
	assert.Equal(
		t,
		"Invoke $roborev-fix. If Roborev issues are found, fix them, "+
			"then continue the task you were doing before this hook interrupted you.",
		PostToolUseAdditionalContext("Invoke $roborev-fix."),
	)
}

func TestPostToolUseAdditionalContextUsesFallback(t *testing.T) {
	assert.Equal(t, postToolUseContinuationInstruction, PostToolUseAdditionalContext(""))
}

// If policy is appended before the continuation instruction, the hook's own
// workflow text can override or dilute the user's final policy.
func TestStopReasonWithFixGuidelinesEndsWithPolicy(t *testing.T) {
	got := StopReasonWithFixGuidelines("Resolve reviews.", "Verify before editing.")
	assert.True(t, strings.HasSuffix(got, "Verify before editing."))
	assert.Contains(t, got, continuationInstruction)
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
