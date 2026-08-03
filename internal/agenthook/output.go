package agenthook

import "strings"

const continuationInstruction = "If Roborev issues are found, fix them, " +
	"then continue the task you were doing before this hook interrupted you."

func BuildOutput(input Input, resp Response) map[string]any {
	if !resp.Triggered {
		return map[string]any{}
	}
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": withContinuationInstruction(resp.Reason),
			},
		}
	}
	return map[string]any{
		"decision": "block",
		"reason":   withContinuationInstruction(resp.Reason),
	}
}

func withContinuationInstruction(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return continuationInstruction
	}
	return reason + " " + continuationInstruction
}
