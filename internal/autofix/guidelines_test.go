package autofix

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// If empty policy formatting drifts, every existing hook and fix prompt changes
// even though the user did not opt in.
func TestAppendGuidelinesPreservesTextWhenEmpty(t *testing.T) {
	base := "existing output\n"
	assert.Equal(t, base, AppendGuidelines(base, " \n\t"))
}

// If composition order drifts, review output can appear after trusted user
// policy and weaken the policy's authority in the final agent prompt.
func TestAppendGuidelinesPlacesTrimmedPolicyLast(t *testing.T) {
	policy := "  Verify every finding.\n  Explain skipped changes.  "
	got := AppendGuidelines("review reminder", policy)

	assert.Equal(t, 1, strings.Count(got, "Verify every finding."))
	assert.True(t, strings.HasSuffix(got, "Verify every finding.\n  Explain skipped changes."))
	assert.Contains(t, got, GuidelinesHeading)
}
