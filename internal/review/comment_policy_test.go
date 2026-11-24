package review

import (
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// A cap must select the most severe eligible findings, preserving ties and the source.
func TestCommentMaxFindings(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Problem: "low-one"},
		{Severity: "high", Problem: "high-one"},
		{Severity: "critical", Problem: "critical-one"},
		{Severity: "high", Problem: "high-two"},
	}}
	original := slices.Clone(doc.Findings)
	for _, tc := range []struct {
		cap     int
		want    []string
		omitted string
	}{
		{0, []string{"critical-one", "high-one", "high-two"}, ""},
		{1, []string{"critical-one"}, "2 more findings in the full review: `roborev show --job 123`"},
		{2, []string{"critical-one", "high-one"}, "1 more finding in the full review: `roborev show --job 123`"},
		{9, []string{"critical-one", "high-one", "high-two"}, ""},
	} {
		t.Run(fmt.Sprint(tc.cap), func(t *testing.T) {
			prepared := PrepareComment(CommentConfig{MinSeverity: "high", MaxFindings: tc.cap, FullReviewReference: "roborev show --job 123"}, ReviewResult{Structured: &doc})
			var got []string
			for _, f := range prepared.findings {
				got = append(got, f.problem)
			}
			assert.Equal(t, tc.want, got)
			body := FormatComment(prepared)
			if tc.omitted != "" {
				assert.Contains(t, body, tc.omitted)
			} else {
				assert.NotContains(t, body, "more finding")
			}
			assert.Equal(t, original, doc.Findings)
			assert.False(t, prepared.IsClean())
		})
	}
	prose := "### High\nFirst\n\n### High\nSecond"
	assert.Equal(t, prose, FormatComment(PrepareComment(CommentConfig{MaxFindings: 1}, ReviewResult{Output: prose})))
	assert.Contains(t, FormatComment(PrepareComment(CommentConfig{MinSeverity: "high", MaxFindings: 1}, ReviewResult{Output: prose})), "Second")
}

func TestCommentCleanMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result ReviewResult
		clean  bool
	}{
		{"legacy filtered", ReviewResult{StructuredOutput: jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"### Low\nMinor","recorded_verdict":false}}`)}, false},
		{"structured empty", ReviewResult{Structured: &StructuredReview{}}, true},
		{"structured filtered", ReviewResult{Structured: &StructuredReview{Findings: []StructuredFinding{{Severity: "low", Problem: "minor"}}}}, true},
		{"parsed prose filtered", ReviewResult{Output: "### Low\nMinor"}, true},
		{"unlabelled survives", ReviewResult{Output: "Unlabelled concern\n\n### Low\nMinor"}, false},
		{"clean looking prose", ReviewResult{Output: "No issues found."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.clean, PrepareComment(CommentConfig{MinSeverity: "high"}, tc.result).IsClean())
		})
	}
}

func FuzzCommentMaxFindings(f *testing.F) {
	f.Add([]byte{0, 3, 1, 3, 2}, uint8(2), uint8(1))
	f.Add([]byte{}, uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, capByte, thresholdByte uint8) {
		severities := []string{"low", "medium", "high", "critical"}
		var doc StructuredReview
		for i, b := range data {
			doc.Findings = append(doc.Findings, StructuredFinding{Severity: severities[int(b)%4], Problem: fmt.Sprint(i)})
		}
		cap := int(capByte)
		threshold := severities[int(thresholdByte)%4]
		got := PrepareComment(CommentConfig{MinSeverity: threshold, MaxFindings: cap}, ReviewResult{Structured: &doc})
		// Independently build the expected stable severity buckets from the input.
		var want []string
		for rank := 3; rank >= int(thresholdByte)%4; rank-- {
			for i, b := range data {
				if int(b)%4 == rank {
					want = append(want, fmt.Sprint(i))
				}
			}
		}
		if cap > 0 && len(want) > cap {
			want = want[:cap]
		}
		require.Len(t, got.findings, len(want))
		for i, w := range want {
			assert.Equal(t, w, got.findings[i].problem)
			assert.GreaterOrEqual(t, config.SeverityRank(got.findings[i].severity), config.SeverityRank(threshold))
		}
	})
}

func TestCommentMinConfidence(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "high", Problem: "legacy confidence", Confidence: 0},
		{Severity: "high", Problem: "explicit zero", Kind: "defect", Confidence: 0},
		{Severity: "high", Problem: "below floor", Kind: "defect", Confidence: 0.69},
		{Severity: "high", Problem: "at floor", Kind: "defect", Confidence: 0.7},
		{Severity: "high", Problem: "confident", Kind: "defect", Confidence: 0.9},
		{Severity: "critical", Problem: "critical", Kind: "defect", Confidence: 0.7},
	}}
	original := slices.Clone(doc.Findings)
	for _, tc := range []struct {
		name    string
		floor   float64
		cap     int
		want    []string
		omitted int
	}{
		{"off", 0, 0, []string{"critical", "legacy confidence", "explicit zero", "below floor", "at floor", "confident"}, 0},
		{"inclusive floor", 0.7, 0, []string{"critical", "legacy confidence", "at floor", "confident"}, 0},
		{"cap after filters", 0.7, 3, []string{"critical", "legacy confidence", "at floor"}, 1},
		{"missing survives", 1, 0, []string{"legacy confidence"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := PrepareComment(CommentConfig{MinSeverity: "high", MinConfidence: tc.floor, MaxFindings: tc.cap}, ReviewResult{Structured: &doc})
			var got []string
			for _, finding := range prepared.findings {
				got = append(got, finding.problem)
			}
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.omitted, prepared.omitted)
			assert.Equal(t, original, doc.Findings)
		})
	}
	for _, tc := range []struct {
		name        string
		minSeverity string
		findings    []StructuredFinding
		message     string
	}{
		{name: "already clean", message: "No issues found."},
		{name: "severity only", minSeverity: "high", findings: []StructuredFinding{{Severity: "low", Kind: "defect", Confidence: 0}}, message: "No findings at or above high severity."},
		{name: "confidence removed eligible finding", minSeverity: "high", findings: []StructuredFinding{{Severity: "high", Kind: "defect", Confidence: 0}}, message: "No findings match the comment filters."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared := PrepareComment(CommentConfig{MinSeverity: tc.minSeverity, MinConfidence: 0.7}, ReviewResult{Structured: &StructuredReview{Findings: tc.findings}})
			assert.True(t, prepared.IsClean())
			assert.Equal(t, "**Verdict:** "+tc.message+"\n", FormatComment(prepared))
		})
	}
}

func TestCommentFold(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "medium", Problem: "defect"},
		{Severity: "low", Problem: "naming issue", Unlikely: true},
		{Severity: "high", Problem: "rare", Unlikely: true},
		{Severity: "medium", Problem: "another"},
	}}
	prepared := PrepareComment(CommentConfig{Fold: true, MinSeverity: "low", MaxFindings: 1}, ReviewResult{Structured: &doc})
	body := FormatComment(prepared)
	assert.Contains(t, body, "Changes require fixes for 1 finding")
	assert.Contains(t, body, "1 nit and 1 unlikely finding hidden")
	assert.Contains(t, body, "rare")
	assert.Contains(t, body, "naming issue")
	assert.Contains(t, body, "1 more finding in the full review")
	assert.Len(t, doc.Findings, 4)
	filteredSeverity := FormatComment(PrepareComment(CommentConfig{Fold: true, MinSeverity: "medium"}, ReviewResult{Structured: &doc}))
	assert.NotContains(t, filteredSeverity, "naming issue")
	assert.Contains(t, filteredSeverity, "1 unlikely finding hidden")

	all := FormatComment(PrepareComment(CommentConfig{ShowAll: true, MinSeverity: "high", MinConfidence: 1, MaxFindings: 1}, ReviewResult{Structured: &doc}))
	assert.NotContains(t, all, "<details>")
	assert.Contains(t, all, "4 findings")
	folded := PrepareComment(CommentConfig{Fold: true}, ReviewResult{Structured: &StructuredReview{Findings: doc.Findings[1:3]}})
	assert.True(t, folded.IsClean())
	assert.Contains(t, FormatComment(folded), "No findings need fixes before merge")
	confidenceDoc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Kind: "improvement", Confidence: .4, Problem: "filtered nit"},
		{Severity: "high", Kind: "defect", Confidence: .4, Unlikely: true, Problem: "filtered rare"},
		{Severity: "low", Kind: "defect", Confidence: .88, Problem: "boundary nit"},
		{Severity: "high", Problem: "unscored", Unlikely: true},
	}}
	filtered := FormatComment(PrepareComment(CommentConfig{Fold: true, MinSeverity: "low", MinConfidence: .88, MaxFindings: 1}, ReviewResult{Structured: &confidenceDoc}))
	assert.NotContains(t, filtered, "filtered nit")
	assert.NotContains(t, filtered, "filtered rare")
	assert.Contains(t, filtered, "boundary nit")
	assert.Contains(t, filtered, "unscored")
	showAll := FormatComment(PrepareComment(CommentConfig{ShowAll: true, MinSeverity: "high", MinConfidence: .95, MaxFindings: 1}, ReviewResult{Structured: &confidenceDoc}))
	assert.Contains(t, showAll, "filtered nit")
	assert.Contains(t, showAll, "filtered rare")
	assert.Contains(t, showAll, "4 findings")
	prose := "### High\nHigh issue.\n\n### Low\nLow issue."
	policy := CommentConfig{MinSeverity: "medium", ShowAll: true}
	for _, result := range []ReviewResult{
		{Output: prose},
		{StructuredOutput: jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"### High\nHigh issue.\n\n### Low\nLow issue.","recorded_verdict":false}}`)},
	} {
		output := FormatComment(PrepareComment(policy, result))
		assert.Contains(t, output, "High issue")
		assert.NotContains(t, output, "Low issue")
	}

}
