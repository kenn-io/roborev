package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func memberReview(problems ...string) review.ReviewResult {
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion}
	for _, p := range problems {
		doc.Findings = append(doc.Findings, structuredreview.Finding{Severity: "medium", Kind: "defect", Problem: p, Fix: "fix"})
	}
	return review.ReviewResult{Agent: "codex", Status: review.ResultDone, Structured: &doc}
}

func TestReplaceInstructionsKeepsMemberReviews(t *testing.T) {
	reviews := []review.ReviewResult{memberReview("Nil map write."), memberReview("Leaked file handle.")}

	prompt, err := replaceInstructions(review.BuildSynthesisPrompt(reviews, "low"), "Keep every finding.\n")
	require.NoError(t, err)

	assert := assert.New(t)
	assert.True(strings.HasPrefix(prompt, "Keep every finding.\n\n---\n### Review 1"))
	assert.Contains(prompt, "Nil map write.")
	assert.Contains(prompt, "### Review 2")
	assert.Contains(prompt, "Leaked file handle.")
	assert.NotContains(prompt, "Deduplicate findings reported by multiple reviewers")
}

func TestConcatenateKeepsEveryMemberFindingWithItsSource(t *testing.T) {
	raw, err := concatenate([]review.ReviewResult{memberReview("A.", "B."), memberReview("A.")})
	require.NoError(t, err)

	doc, err := review.DecodeSynthesisDocument(raw, make([]review.ReviewResult, 2))
	require.NoError(t, err)
	require.Len(t, doc.Findings, 3)
	assert.Equal(t, []int{1}, doc.Findings[1].Sources)
	assert.Equal(t, []int{2}, doc.Findings[2].Sources)
	assert.Equal(t, "A.", doc.Findings[2].Problem)
}

func TestAccountForIDsRestoresOmittedFindings(t *testing.T) {
	first := memberReview("Nil map write in save.", "Unused import.")
	second := memberReview("save writes to a nil map when the cache is empty, panicking.")
	second.Structured.Findings[0].Severity = "high"
	reviews := []review.ReviewResult{first, second}
	agent := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Verdict: "fail", Findings: []structuredreview.Finding{
		{Severity: "medium", Problem: "[R1.1, R2.1] Merged nil map write.", Fix: "make the map", Sources: []int{1, 2}},
		{Severity: "low", Problem: "Something no member said.", Fix: "x", Sources: []int{1}},
	}}

	verbatim, restored, invented := accountForIDs(agent, reviews, true)
	assert := assert.New(t)
	assert.Equal(1, restored)
	assert.Equal(1, invented)
	require.Len(t, verbatim.Findings, 2)
	assert.Equal("save writes to a nil map when the cache is empty, panicking.", verbatim.Findings[0].Problem)
	assert.Equal("high", verbatim.Findings[0].Severity)
	assert.Equal([]int{1, 2}, verbatim.Findings[0].Sources)
	assert.Equal("Unused import.", verbatim.Findings[1].Problem)
	assert.Equal([]int{1}, verbatim.Findings[1].Sources)

	rewritten, _, _ := accountForIDs(agent, reviews, false)
	assert.Equal("Merged nil map write.", rewritten.Findings[0].Problem)
	assert.Equal("high", rewritten.Findings[0].Severity)
}

func TestRenderWithIDsNumbersEveryFinding(t *testing.T) {
	text := renderWithIDs([]review.ReviewResult{memberReview("A.", "B."), memberReview()})
	assert.Contains(t, text, "R1.1 [medium, defect]\nProblem: A.")
	assert.Contains(t, text, "R1.2 [medium, defect]\nProblem: B.")
	assert.Contains(t, text, "### Review 2\nNo findings.")
}
