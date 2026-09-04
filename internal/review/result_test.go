package review

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestTrimPartialRune(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"ascii only", "hello", "hello"},
		{
			"clean emoji boundary",
			"abc😀",
			"abc😀",
		},
		{
			"split 4-byte emoji after 1 byte",
			"abc" + string([]byte{0xF0}),
			"abc",
		},
		{
			"split 4-byte emoji after 2 bytes",
			"abc" + string([]byte{0xF0, 0x9F}),
			"abc",
		},
		{
			"split 4-byte emoji after 3 bytes",
			"abc" + string([]byte{0xF0, 0x9F, 0x98}),
			"abc",
		},
		{
			"split 2-byte char after 1 byte",
			"abc" + string([]byte{0xC3}),
			"abc",
		},
		{
			"interior invalid bytes preserved",
			"a" + string([]byte{0xFF}) + "b",
			"a" + string([]byte{0xFF}) + "b",
		},
		{
			"orphan continuation byte",
			"abc" + string([]byte{0x80}),
			"abc",
		},
		{
			"two orphan continuation bytes",
			"abc" + string([]byte{0x80, 0x80}),
			"abc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimPartialRune(tt.in)
			assert.Equalf(tt.want, got, "TrimPartialRune(%q) = %q, want %q", tt.in, got, tt.want)
		})
	}
}

func TestTrimPartialRune_NoFullStringScan(t *testing.T) {
	assert := assert.New(t)

	// Verify that a string with interior invalid UTF-8 is NOT
	// stripped down to empty — only the trailing boundary matters.
	// This is the bug that utf8.ValidString would cause.
	interior := strings.Repeat("x", 1000) +
		string([]byte{0xFF}) +
		strings.Repeat("y", 1000)
	got := TrimPartialRune(interior)
	assert.Equalf(interior, got, "interior invalid bytes should be preserved, got len %d want len %d", len(got), len(interior))
}

func TestHasSubstantiveOutput(t *testing.T) {
	tests := []struct {
		name    string
		results []ReviewResult
		want    bool
	}{
		{name: "empty batch"},
		{
			name: "completed output",
			results: []ReviewResult{{
				Status: ResultDone,
				Output: "## Findings\n",
			}},
			want: true,
		},
		{
			name: "completed whitespace",
			results: []ReviewResult{{
				Status: ResultDone,
				Output: " \n\t",
			}},
		},
		{
			name: "completed empty-output placeholder",
			results: []ReviewResult{{
				Status: ResultDone,
				Output: "No review output generated",
			}},
		},
		{
			name: "failed output",
			results: []ReviewResult{{
				Status: ResultFailed,
				Output: "partial diagnostics",
			}},
		},
		{
			name: "skipped output",
			results: []ReviewResult{{
				Status: ResultSkipped,
				Output: "not a review",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HasSubstantiveOutput(tt.results))
		})
	}
}

func TestUnavailableError(t *testing.T) {
	assert.Equal(t,
		UnavailableErrorPrefix+"agent review: native package missing",
		UnavailableError("agent review: native package missing"),
	)
	assert.Equal(t,
		UnavailableErrorPrefix+"already categorized",
		UnavailableError(UnavailableErrorPrefix+"already categorized"),
	)
}

func TestApplyMinSeverityProseReparsesUnderNewThreshold(t *testing.T) {
	assert := assert.New(t)
	result := ReviewResult{
		Status:      ResultDone,
		Output:      "Summary.\n\n- Low: naming nit\n",
		Verdict:     storage.VerdictFail,
		MinSeverity: "low",
	}

	same := result.ApplyMinSeverity("")
	assert.Equal(storage.VerdictFail, same.Verdict, "an empty threshold keeps the result's own")
	assert.Equal("low", same.MinSeverity)

	stricter := result.ApplyMinSeverity("medium")
	assert.Equal(storage.VerdictPass, stricter.Verdict)
	assert.Equal("medium", stricter.MinSeverity)
	assert.Equal(result.Output, stricter.Output, "prose output is never rewritten")

	looser := stricter.ApplyMinSeverity("low")
	assert.Equal(storage.VerdictFail, looser.Verdict, "a looser panel threshold applies as configured")
	assert.Equal("low", looser.MinSeverity)

	failed := ReviewResult{Status: ResultFailed, Error: "boom"}.ApplyMinSeverity("high")
	assert.Equal(storage.VerdictUnknown, failed.Verdict)
	assert.Empty(failed.MinSeverity, "failed results are returned unchanged")
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

	// A member decided under "critical" still fails a CI panel whose
	// threshold is "medium": the panel threshold is the lowest severity that
	// fails the combined review, not a floor under the member's own.
	critical := result.ApplyMinSeverity("critical")
	assert.Equal(storage.VerdictPass, critical.Verdict)
	combined := critical.ApplyMinSeverity("medium")
	assert.Equal(storage.VerdictFail, combined.Verdict)
	assert.Equal("medium", combined.MinSeverity)
	assert.NotContains(combined.Output, "No findings at or above")
}
