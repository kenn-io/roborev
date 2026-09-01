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
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": PostToolUseAdditionalContext(InstructionWithFixSessionCompletion(resp)),
			},
		}
	}
	return map[string]any{
		"decision": "block",
		"reason":   StopReason(InstructionWithFixSessionCompletion(resp)),
	}
}

func BuildOutputWithFixGuidelines(input Input, resp Response, guidelines string) map[string]any {
	if !resp.Triggered {
		return BuildOutput(input, resp)
	}
	if resp.TriggeredBy == "fix_session" {
		return BuildOutput(input, resp)
	}
	instruction := InstructionWithFixSessionCompletion(resp)
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": PostToolUseAdditionalContextWithFixGuidelines(instruction, guidelines),
			},
		}
	}
	return map[string]any{
		"decision": "block",
		"reason":   StopReasonWithFixGuidelines(instruction, guidelines),
	}
}

func InstructionWithFixSessionCompletion(resp Response) string {
	instruction := resolvedInstruction(resp.Reason)
	if resp.FixSessionID == nil {
		return instruction
	}
	bareCommand := "roborev agent-hook fix-done " + resp.FixSessionID.String()
	command := resp.FixDoneCommand
	if command == "" {
		command = bareCommand
	}
	if strings.Contains(instruction, bareCommand) {
		return strings.Replace(instruction, bareCommand, command, 1)
	}
	return instruction + "\n\nAfter completing this Agent Hook fix, run `" + command + "`."
}

func resolvedInstruction(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return DefaultInstruction
	}
	return reason
}
