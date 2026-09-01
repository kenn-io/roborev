package autofix

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatReviewedRef(t *testing.T) {
	assert.Empty(t, FormatReviewedRef(" \n\t"))
	assert.Equal(t, "Reviewed git ref: \"abc123\".\n\n", FormatReviewedRef(" abc123 "))
	assert.Equal(t, "Reviewed git ref: \"abc\\n123\".\n\n", FormatReviewedRef("abc\n123"))
}

func TestRestorationHistoryGuidancePreservesExistingIdentity(t *testing.T) {
	assert := assert.New(t)
	assert.Contains(RestorationHistoryGuidance, "inspect the relevant repository history")
	assert.Contains(RestorationHistoryGuidance, "schema object names")
	assert.Contains(RestorationHistoryGuidance, "migration semantics")
	assert.Contains(RestorationHistoryGuidance, "current checkout")
}
