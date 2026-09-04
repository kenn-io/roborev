package review

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestApplyMinSeverityProseReparsesUnderStricterThreshold(t *testing.T) {
	assert := assert.New(t)
	result := ReviewResult{
		Status:      ResultDone,
		Output:      "Summary.\n\n- Low: naming nit\n",
		Verdict:     storage.VerdictFail,
		MinSeverity: "low",
	}

	relaxed := result.ApplyMinSeverity("")
	assert.Equal(storage.VerdictFail, relaxed.Verdict, "a looser threshold never relaxes a verdict")
	assert.Equal("low", relaxed.MinSeverity)

	stricter := result.ApplyMinSeverity("medium")
	assert.Equal(storage.VerdictPass, stricter.Verdict)
	assert.Equal("medium", stricter.MinSeverity)
	assert.Equal(result.Output, stricter.Output, "prose output is never rewritten")

	failed := ReviewResult{Status: ResultFailed, Error: "boom"}.ApplyMinSeverity("high")
	assert.Equal(storage.VerdictUnknown, failed.Verdict)
	assert.Empty(failed.MinSeverity)
}

func TestApplyMinSeverityStructuredRerendersEveryFinding(t *testing.T) {
	assert := assert.New(t)
	structured := StructuredReview{
		SchemaVersion: storage.StructuredReviewSchemaVersion,
		Summary:       "Two findings.",
		Findings: []StructuredFinding{
			{Severity: "medium", Problem: "Off by one.", Fix: "Fix the bound."},
			{Severity: "low", Problem: "Name is vague.", Fix: "Rename it."},
		},
	}
	result := ReviewResult{Status: ResultDone, Structured: &structured}

	medium := result.ApplyMinSeverity("medium")
	assert.Equal(storage.VerdictFail, medium.Verdict)
	assert.Contains(medium.Output, "Off by one.")
	assert.Contains(medium.Output, "Name is vague.")

	high := medium.ApplyMinSeverity("high")
	assert.Equal(storage.VerdictPass, high.Verdict)
	assert.Equal("high", high.MinSeverity)
	assert.Contains(high.Output, "No findings at or above high severity.")
	assert.Contains(high.Output, "Off by one.")

	assert.Equal(storage.VerdictPass, high.ApplyMinSeverity("medium").Verdict,
		"the stricter threshold already applied wins")
}
