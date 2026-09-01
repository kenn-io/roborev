package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildFixPromptWithInstructionsIncludesRestorationContext(t *testing.T) {
	prompt := buildFixPromptWithInstructions(
		"Restore the removed database constraint.",
		"Keep the change narrowly scoped.",
		"",
		nil,
		"abc123def456",
	)

	assert := assert.New(t)
	assert.Contains(prompt, "inspect the relevant repository history")
	assert.Contains(prompt, "Preserve established identifiers")
	assert.Contains(prompt, "Reviewed git ref: \"abc123def456\".")
	assert.Contains(prompt, "Keep the change narrowly scoped.")
}
