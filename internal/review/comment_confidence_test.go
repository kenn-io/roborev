package review

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentMinConfidence(t *testing.T) {
	floor := 0.7
	original := StructuredReview{SchemaVersion: 3, Verdict: "fail", Findings: []StructuredFinding{
		{Severity: "high", Kind: "defect", Confidence: 0.6, Problem: "Below", Sources: []int{1}},
		{Severity: "high", Kind: "defect", Confidence: 0.7, Problem: "Equal", Sources: []int{1}},
		{Severity: "high", Kind: "defect", Confidence: 0.9, Problem: "Above", Sources: []int{1}},
		{Severity: "high", Problem: "Missing", Sources: []int{1}},
		{Severity: "high", Kind: "defect", Confidence: 0, Problem: "Zero", Sources: []int{1}},
	}}
	for _, tc := range []struct {
		name  string
		floor *float64
		want  []string
	}{
		{"unset", nil, []string{"Below", "Equal", "Above", "Missing", "Zero"}},
		{"positive", &floor, []string{"Equal", "Above", "Missing"}},
		{"zero reset", new(float64), []string{"Below", "Equal", "Above", "Missing", "Zero"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := original
			doc.Findings = slices.Clone(original.Findings)
			for i := range doc.Findings {
				doc.Findings[i].Sources = slices.Clone(original.Findings[i].Sources)
			}
			before := doc.Markdown("")
			comment := PrepareComment(CommentConfig{MinConfidence: tc.floor}, ReviewResult{Structured: &doc})
			var problems []string
			for _, f := range comment.findings {
				problems = append(problems, f.problem)
			}
			assert := assert.New(t)
			assert.Equal(tc.want, problems)
			assert.Equal(original, doc)
			assert.Equal(before, doc.Markdown(""))
			assert.Equal("fail", doc.Verdict)
		})
	}
	// Filtering changes only presentation, including the all-hidden message.
	empty := original
	empty.Findings = empty.Findings[:1]
	body := FormatComment(PrepareComment(CommentConfig{MinConfidence: &floor, MinSeverity: "critical"}, ReviewResult{Structured: &empty}))
	assert.Contains(t, body, "No findings match the comment filters")
	assert.NotContains(t, body, "No issues found")
	prose := "A finding with no stated confidence."
	assert.Equal(t, prose, FormatComment(PrepareComment(CommentConfig{MinConfidence: &floor}, ReviewResult{Output: prose})))
}

func TestCommentMinConfidenceDecoded(t *testing.T) {
	floor := 0.7
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"v1", `{"schema_version":1,"summary":"s","findings":[{"severity":"high","problem":"Historical","fix":"f","location":"a.go:3"}]}`, []string{"Historical"}},
		{"v2", `{"schema_version":2,"summary":"s","verdict":"fail","findings":[{"severity":"high","problem":"Historical","fix":"f","location":null}]}`, []string{"Historical"}},
		{"v3 missing", `{"schema_version":3,"summary":"s","verdict":"fail","findings":[{"severity":"high","problem":"Historical","fix":"f"}]}`, []string{"Historical"}},
		{"v3 explicit zero", `{"schema_version":3,"summary":"s","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0,"problem":"Zero","fix":"f"}]}`, nil},
		{"v3 equal", `{"schema_version":3,"summary":"s","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0.7,"problem":"Equal","fix":"f"}]}`, []string{"Equal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := DecodeStructuredReview([]byte(tc.raw))
			require.NoError(t, err)
			for _, r := range []ReviewResult{{Structured: &doc}, {StructuredOutput: []byte(tc.raw)}} {
				c := PrepareComment(CommentConfig{MinConfidence: &floor}, r)
				var got []string
				for _, f := range c.findings {
					got = append(got, f.problem)
				}
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestCommentPublicationRanking(t *testing.T) {
	doc := StructuredReview{SchemaVersion: 3, Summary: "Review complete.", Verdict: "fail", SourceLabels: []string{"first", "second", "third"}, Findings: []StructuredFinding{
		{Severity: "high", Kind: "defect", Problem: "one source", Fix: "fix", Confidence: 1, Sources: []int{1}},
		{Severity: "high", Kind: "defect", Problem: "tie first", Fix: "fix", Confidence: 0.8, Sources: []int{1, 2}},
		{Severity: "low", Kind: "defect", Problem: "low severity", Fix: "fix", Confidence: 1, Sources: []int{1, 2, 3}},
		{Severity: "high", Kind: "defect", Problem: "tie second", Fix: "fix", Confidence: 0.8, Sources: []int{1, 2}},
		{Severity: "high", Kind: "defect", Problem: "more confident", Fix: "fix", Confidence: 0.9, Sources: []int{1, 2}},
		{Severity: "critical", Problem: "critical severity", Fix: "fix", Sources: []int{1}},
		// These match tidyFindings' merge predicate but publication must keep both.
		{Severity: "medium", Problem: "independent", File: "a.go", Fix: "fix", Sources: []int{1}},
		{Severity: "medium", Problem: "independent", File: "a.go", Fix: "fix", Sources: []int{2}},
	}}
	original := doc
	original.SourceLabels = slices.Clone(doc.SourceLabels)
	original.Findings = slices.Clone(doc.Findings)
	for i := range original.Findings {
		original.Findings[i].Sources = slices.Clone(doc.Findings[i].Sources)
	}
	before := doc.Markdown("")
	ranked := PrepareComment(CommentConfig{RankFindings: true}, ReviewResult{Structured: &doc})
	var got []string
	for _, f := range ranked.findings {
		got = append(got, f.problem)
	}
	assert.Equal(t, []string{"critical severity", "more confident", "tie first", "tie second", "one source", "independent", "independent", "low severity"}, got)
	assert.Equal(t, original, doc)
	assert.Equal(t, before, doc.Markdown(""))
	assert.Equal(t, []int{1, 2}, doc.Findings[1].Sources)
	ordinary := PrepareComment(CommentConfig{}, ReviewResult{Structured: &doc})
	assert.Equal(t, "one source", ordinary.findings[0].problem, "shared defaults retain original order")
	assert.Len(t, doc.Findings, 8)
}

func FuzzCommentMinConfidence(f *testing.F) {
	f.Add(0.7, 0.5, true)
	f.Add(0.0, 0.0, true)
	f.Add(1.0, 0.0, false)
	f.Fuzz(func(t *testing.T, floor, confidence float64, hasConfidence bool) {
		if math.IsNaN(floor) || math.IsInf(floor, 0) || floor < 0 || floor > 1 || math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence < 0 || confidence > 1 {
			t.Skip()
		}
		finding := StructuredFinding{Severity: "high", Problem: fmt.Sprintf("confidence %g", confidence), Fix: "fix"}
		if hasConfidence {
			finding.Kind = "defect"
			finding.Confidence = confidence
		}
		doc := StructuredReview{SchemaVersion: 3, Summary: "Review complete.", Verdict: "fail", Findings: []StructuredFinding{finding}}
		c := PrepareComment(CommentConfig{MinConfidence: &floor}, ReviewResult{Structured: &doc})
		assert.Equal(t, !hasConfidence || confidence >= floor, len(c.findings) == 1)
	})
}
