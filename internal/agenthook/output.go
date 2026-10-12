package agenthook

import (
	"strings"

	"go.kenn.io/roborev/internal/autofix"
)

func PostToolUseAdditionalContext(reason string) string {
	return resolvedInstruction(reason)
}

func StopReason(reason string) string {
	return resolvedInstruction(reason)
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
	if injectsAdditionalContext(input.HookEventName) {
		return additionalContextOutput(input.HookEventName, PostToolUseAdditionalContext(resp.Reason))
	}
	if input.HookEventName == "PostToolUse" {
		return additionalContextOutput("PostToolUse", PostToolUseAdditionalContext(resp.Reason))
	}
	return map[string]any{"decision": "block", "reason": StopReason(resp.Reason)}
}

func BuildOutputWithFixGuidelines(input Input, resp Response, guidelines string) map[string]any {
	if !resp.Triggered {
		return BuildOutput(input, resp)
	}
	if injectsAdditionalContext(input.HookEventName) {
		return additionalContextOutput(
			input.HookEventName,
			PostToolUseAdditionalContextWithFixGuidelines(resp.Reason, guidelines),
		)
	}
	if input.HookEventName == "PostToolUse" {
		return additionalContextOutput(
			"PostToolUse",
			PostToolUseAdditionalContextWithFixGuidelines(resp.Reason, guidelines),
		)
	}
	return map[string]any{
		"decision": "block",
		"reason":   StopReasonWithFixGuidelines(resp.Reason, guidelines),
	}
}

// injectsAdditionalContext reports whether the event delivers hook output as
// injected context for the model rather than a lifecycle decision. ZCode
// schedules only UserPromptSubmit and SessionStart today, and both take
// additionalContext.
func injectsAdditionalContext(eventName string) bool {
	return eventName == "UserPromptSubmit" || eventName == "SessionStart"
}

func additionalContextOutput(eventName, context string) map[string]any {
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     eventName,
			"additionalContext": context,
		},
	}
}

func resolvedInstruction(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DefaultInstruction
	}
	return reason
}
