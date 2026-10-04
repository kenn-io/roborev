package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
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

func TestBuildFixPromptSeparatesPreviousPlans(t *testing.T) {
	body := buildFixPromptWithInstructions("Missing cancellation", "", "", []storage.Response{
		{Responder: "roborev-plan", Response: "Proposed cancellation check"},
	}, "")
	assert := assert.New(t)
	assert.Contains(body, "## Previous Plans")
	assert.Contains(body, "Proposed cancellation check")
	assert.NotContains(body, "## Previous Addressing Attempts")
	assert.NotContains(body, "## User Comments")
}
