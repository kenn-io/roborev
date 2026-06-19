package agenthook

import "strings"

const postToolUseContinuationInstruction = "If Roborev issues are found, fix them, " +
	"then continue the task you were doing before this hook interrupted you."

func BuildOutput(input Input, resp Response) map[string]any {
	if !resp.Triggered {
		return map[string]any{}
	}
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": postToolUseAdditionalContext(resp.Reason),
			},
		}
	}
	return map[string]any{
		"decision": "block",
		"reason":   resp.Reason,
	}
}

func postToolUseAdditionalContext(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return postToolUseContinuationInstruction
	}
	return reason + " " + postToolUseContinuationInstruction
}
