package agenthook

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPostToolUseAdditionalContextContinuesInterruptedTask(t *testing.T) {
	assert.Equal(
		t,
		"Invoke $roborev-fix. If Roborev issues are found, fix them, "+
			"then continue the task you were doing before this hook interrupted you.",
		PostToolUseAdditionalContext("Invoke $roborev-fix."),
	)
}

func TestPostToolUseAdditionalContextUsesFallback(t *testing.T) {
	assert.Equal(t, postToolUseContinuationInstruction, PostToolUseAdditionalContext(""))
}
