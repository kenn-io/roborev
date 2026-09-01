package agenthook

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
