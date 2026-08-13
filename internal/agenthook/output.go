package agenthook

import (
	"strings"

	"go.kenn.io/roborev/internal/autofix"
)

const continuationInstruction = "If Roborev issues are found, fix them, " +
	"then continue the task you were doing before this hook interrupted you."

const postToolUseContinuationInstruction = continuationInstruction

func PostToolUseAdditionalContext(reason string) string {
	return withContinuationInstruction(reason)
}

func StopReason(reason string) string {
	return withContinuationInstruction(reason)
}

func PostToolUseAdditionalContextWithFixGuidelines(reason, guidelines string) string {
	return autofix.AppendGuidelines(PostToolUseAdditionalContext(reason), guidelines)
}

func StopReasonWithFixGuidelines(reason, guidelines string) string {
	return autofix.AppendGuidelines(StopReason(reason), guidelines)
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

func BuildOutputWithFixGuidelines(input Input, resp Response, guidelines string) map[string]any {
	if !resp.Triggered {
		return BuildOutput(input, resp)
	}
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": PostToolUseAdditionalContextWithFixGuidelines(resp.Reason, guidelines),
			},
		}
	}
	return map[string]any{
		"decision": "block",
		"reason":   StopReasonWithFixGuidelines(resp.Reason, guidelines),
	}
}

func withContinuationInstruction(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return continuationInstruction
	}
	return reason + " " + continuationInstruction
}
