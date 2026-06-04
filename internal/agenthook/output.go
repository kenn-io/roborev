package agenthook

func BuildOutput(input Input, resp Response) map[string]any {
	if !resp.Triggered {
		return map[string]any{}
	}
	if input.HookEventName == "PostToolUse" {
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "PostToolUse",
				"additionalContext": resp.Reason,
			},
		}
	}
	return map[string]any{
		"decision": "block",
		"reason":   resp.Reason,
	}
}
