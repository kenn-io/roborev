package agenthook

import (
	"encoding/json"
	"strings"
	"testing"
	"uuid"

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

func TestBuildOutputAppendsMatchingFixSessionCompletionCommand(t *testing.T) {
	fixSessionID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	command := "roborev agent-hook fix-done " + fixSessionID.String()

	for _, event := range []string{"Stop", "PostToolUse"} {
		t.Run(event, func(t *testing.T) {
			output := BuildOutputWithFixGuidelines(
				Input{HookEventName: event},
				Response{Triggered: true, Reason: "Resolve reviews.", FixSessionID: new(fixSessionID)},
				"Verify before editing.",
			)
			encoded, err := json.Marshal(output)
			require.NoError(t, err)
			text := string(encoded)
			assert.Equal(t, 1, strings.Count(text, command))
			assert.Less(t, strings.Index(text, command), strings.Index(text, "Verify before editing."))
		})
	}
}

func TestBuildOutputOwnerCloseoutSkipsGuidelinesAndDuplicateCommand(t *testing.T) {
	fixSessionID := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	command := "roborev agent-hook fix-done " + fixSessionID.String()
	reason := "Finish the current Agent Hook fix, then run `" + command + "`."

	output := BuildOutputWithFixGuidelines(
		Input{HookEventName: "Stop"},
		Response{
			Triggered: true, TriggeredBy: "fix_session", Reason: reason,
			FixSessionID: new(fixSessionID),
		},
		"Verify before editing.",
	)

	assert.Equal(t, map[string]any{"decision": "block", "reason": reason}, output)
}
