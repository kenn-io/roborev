package review

import (
	"bytes"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"

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
	for _, tc := range []struct {
		name    string
		cap     int
		floor   string
		doc     StructuredReview
		want    []string
		omitted string
	}{
		{"unlimited", 0, "high", doc, []string{"critical-one", "high-one", "high-two"}, ""},
		{"one", 1, "high", doc, []string{"critical-one"}, "2 more findings in the full review: `roborev show --job 123`"},
		{"two", 2, "high", doc, []string{"critical-one", "high-one"}, "1 more finding in the full review: `roborev show --job 123`"},
		{"above count", 9, "high", doc, []string{"critical-one", "high-one", "high-two"}, ""},
		{"ordinary before unlikely", 1, "", StructuredReview{Findings: []StructuredFinding{
			{Severity: "medium", Problem: "Rare issue", Unlikely: true}, {Severity: "medium", Problem: "Ordinary issue"},
		}}, []string{"Ordinary issue"}, "1 more finding in the full review: `roborev show --job 123`"},
		{"folded findings count toward cap", 1, "", StructuredReview{Findings: []StructuredFinding{
			{Severity: "low", Nit: true, Problem: "First nit"}, {Severity: "high", Problem: "Visible issue"}, {Severity: "low", Nit: true, Problem: "Second nit"},
		}}, []string{"Visible issue"}, "2 more findings in the full review: `roborev show --job 123`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := slices.Clone(tc.doc.Findings)
			prepared := PrepareComment(CommentConfig{MinSeverity: tc.floor, MaxFindings: tc.cap, FullReviewReference: "roborev show --job 123"}, ReviewResult{Structured: &tc.doc})
			var got []string
			for _, f := range prepared.findings {
				got = append(got, f.problem)
			}
			assert.Equal(t, tc.want, got)
			assert.Empty(t, prepared.nits)
			assert.Empty(t, prepared.unlikely)
			body := FormatComment(prepared)
			if tc.omitted != "" {
				assert.Contains(t, body, tc.omitted)
			} else {
				assert.NotContains(t, body, "more finding")
			}
			assert.Equal(t, original, tc.doc.Findings)
			assert.False(t, prepared.IsClean())
		})
	}
	prose := "### High\nFirst\n\n### High\nSecond"
	assert.Equal(t, prose, FormatComment(PrepareComment(CommentConfig{MaxFindings: 1}, ReviewResult{Output: prose})))
	assert.Contains(t, FormatComment(PrepareComment(CommentConfig{MinSeverity: "high", MaxFindings: 1}, ReviewResult{Output: prose})), "Second")
}

func TestCommentSeverityFloorBeforeFolding(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Nit: true, Problem: "Low nit"},
		{Severity: "low", Unlikely: true, Problem: "Low unlikely"},
		{Severity: "medium", Nit: true, Problem: "Medium nit"},
		{Severity: "medium", Unlikely: true, Problem: "Medium unlikely"},
	}}
	body := FormatComment(PrepareComment(CommentConfig{MinSeverity: "medium"}, ReviewResult{Structured: &doc}))
	assert.NotContains(t, body, "Low nit")
	assert.NotContains(t, body, "Low unlikely")
	assert.Contains(t, body, "Medium nit")
	assert.Contains(t, body, "Medium unlikely")
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

func TestCommentCleanWithCappedFoldedFindings(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{{Severity: "low", Problem: "First", Nit: true}, {Severity: "low", Problem: "Second", Nit: true}}}
	prepared := PrepareComment(CommentConfig{MaxFindings: 1}, ReviewResult{Structured: &doc})
	assert.True(t, prepared.IsClean())
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
		{Severity: "low", Problem: "naming issue", Nit: true},
		{Severity: "medium", Problem: "rare", Unlikely: true},
		{Severity: "medium", Problem: "another"},
	}}
	mixed := FormatComment(PrepareComment(CommentConfig{}, ReviewResult{Structured: &doc}))
	assert.Contains(t, mixed, "2 findings shown; 2 folded")
	filteredSeverity := FormatComment(PrepareComment(CommentConfig{MinSeverity: "medium"}, ReviewResult{Structured: &doc}))
	assert.NotContains(t, filteredSeverity, "naming issue")
	assert.Contains(t, filteredSeverity, "1 unlikely finding hidden")

	folded := PrepareComment(CommentConfig{}, ReviewResult{Structured: &StructuredReview{Findings: doc.Findings[1:3]}})
	assert.True(t, folded.IsClean())
	assert.Contains(t, FormatComment(folded), "Changes require fixes before merge.")
	assert.Contains(t, FormatComment(folded), "0 findings shown; 2 folded")
	visible := PrepareComment(CommentConfig{}, ReviewResult{Structured: &StructuredReview{Findings: []StructuredFinding{
		{Severity: "high", Problem: "high rare", Unlikely: true},
		{Severity: "critical", Problem: "critical rare", Unlikely: true},
		{Severity: "low", Topic: "security", Problem: "low security"},
		{Severity: "medium", Topic: "security", Problem: "medium security", Unlikely: true},
	}}})
	assert.False(t, visible.IsClean())
	visibleBody := FormatComment(visible)
	assert.Contains(t, visibleBody, "4 findings")
	assert.NotContains(t, visibleBody, "<details>")
}

func TestCommentShowAll(t *testing.T) {
	confidenceDoc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Kind: "improvement", Confidence: .4, Problem: "filtered nit"},
		{Severity: "high", Kind: "defect", Confidence: .4, Unlikely: true, Problem: "filtered rare"},
		{Severity: "low", Kind: "defect", Confidence: .88, Problem: "boundary nit"},
		{Severity: "high", Problem: "unscored", Unlikely: true},
	}}
	showAll := FormatComment(PrepareComment(CommentConfig{ShowAll: true, MinSeverity: "high", MinConfidence: .95, MaxFindings: 1}, ReviewResult{Structured: &confidenceDoc}))
	assert.Contains(t, showAll, "filtered nit")
	assert.Contains(t, showAll, "filtered rare")
	assert.Contains(t, showAll, "4 findings shown; 0 folded")
}

func TestCommentShowAllKeepsReviewSeverityVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings []StructuredFinding
		clean    bool
		verdict  string
	}{
		{"below floor", []StructuredFinding{{Severity: "low", Problem: "Low issue"}}, true, "No fixes needed before merge."},
		{"mixed", []StructuredFinding{{Severity: "low", Problem: "Low issue"}, {Severity: "high", Problem: "High issue"}}, false, "Changes require fixes before merge."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := StructuredReview{Findings: tc.findings}
			prepared := PrepareComment(CommentConfig{ShowAll: true, MinSeverity: "high"}, ReviewResult{Structured: &doc})
			assert.Equal(t, tc.clean, prepared.IsClean())
			body := FormatComment(prepared)
			assert.Contains(t, body, tc.verdict)
			assert.Contains(t, body, "Low issue")
		})
	}
}

func TestCommentShowAllKeepsProseSeverityFilter(t *testing.T) {
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

func TestCommentFoldEscapesDetailsTags(t *testing.T) {
	for _, tc := range []struct {
		name, problem, fix string
		want               []string
	}{
		{"closing tags", "Before\n</details>\n- Outside", "</DETAILS >", []string{"&lt;/details&gt;", "&lt;/DETAILS &gt;"}},
		{"indented fix", "Issue", "    </details>", []string{"&lt;/details&gt;"}},
		{"nested details", "Before <details open>nested</details> after", "", []string{"&lt;details open&gt;"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := StructuredReview{Findings: []StructuredFinding{{Severity: "low", Nit: true, Problem: tc.problem, Fix: tc.fix}}}
			body := FormatComment(PrepareComment(CommentConfig{}, ReviewResult{Structured: &doc}))
			var rendered bytes.Buffer
			source := []byte(body)
			require.NoError(t, html.New(html.WithUnsafe()).Render(&rendered, source, parser.New().Parse(source)))
			assert.Equal(t, 1, strings.Count(rendered.String(), "<details"))
			assert.Equal(t, 1, strings.Count(rendered.String(), "</details>"))
			assert.NotContains(t, rendered.String(), "&amp;lt;/details&amp;gt;")
			for _, want := range tc.want {
				assert.Contains(t, rendered.String(), want)
			}
		})
	}
}

func TestCommentFoldsClassifiedNits(t *testing.T) {
	doc := StructuredReview{Findings: []StructuredFinding{
		{Severity: "low", Problem: "Data is lost"},
		{Severity: "medium", Problem: "Rename variable", Nit: true},
		{Severity: "low", Problem: "Fix spelling", Nit: true},
	}}
	body := FormatComment(PrepareComment(CommentConfig{MinSeverity: "low"}, ReviewResult{Structured: &doc}))
	assert.Less(t, strings.Index(body, "Data is lost"), strings.Index(body, "<details>"))
	assert.Greater(t, strings.Index(body, "Rename variable"), strings.Index(body, "<details>"))
	assert.Contains(t, body, "2 nits hidden")
	body = FormatComment(PrepareComment(CommentConfig{MinSeverity: "medium"}, ReviewResult{Structured: &doc}))
	assert.NotContains(t, body, "Fix spelling")
	assert.Contains(t, body, "1 nit hidden")
}
