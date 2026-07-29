package agenthook

import "strings"

const postToolUseContinuationInstruction = "If Roborev issues are found, fix them, " +
	"then continue the task you were doing before this hook interrupted you."

func PostToolUseAdditionalContext(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return postToolUseContinuationInstruction
	}
	return reason + " " + postToolUseContinuationInstruction
}
