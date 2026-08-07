package agenthook

import "strings"

const continuationInstruction = "If Roborev issues are found, fix them, " +
	"then continue the task you were doing before this hook interrupted you."

const postToolUseContinuationInstruction = continuationInstruction

func PostToolUseAdditionalContext(reason string) string {
	return withContinuationInstruction(reason)
}

func StopReason(reason string) string {
	return withContinuationInstruction(reason)
}

func BuildOutput(input Input, resp Response) map[string]any {
	if !resp.Triggered {
		return map[string]any{}
	}
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": PostToolUseAdditionalContext(resp.Reason),
			},
		}
	}
	return map[string]any{"decision": "block", "reason": StopReason(resp.Reason)}
}

func withContinuationInstruction(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return continuationInstruction
	}
	return reason + " " + continuationInstruction
}
