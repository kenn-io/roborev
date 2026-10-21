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
