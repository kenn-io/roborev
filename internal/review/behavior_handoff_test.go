package review

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// Losing a clause while preparing synthesis input can hide a trigger,
// preservation constraint, or an explicit limitation on the evidence.
func TestBehaviorHandoffSurvivesSynthesisInput(t *testing.T) {
	property := func(problemText, fixText, instructions string, kindIndex, severityIndex uint8, confidencePercent uint8, neutral bool) bool {
		assert := assert.New(t)
		problem := strings.TrimSpace("Tenant A receives tenant B's invoice when requesting B's ID.\n" + problemText)
		fix := strings.TrimSpace("Reject other-tenant reads; preserve own-invoice access.\nProposed check, not run.\n" + fixText)
		kind := []string{"defect", "improvement"}[kindIndex%2]
		severity := []string{"critical", "high", "medium", "low"}[severityIndex%4]
		confidence := float64(confidencePercent%101) / 100
		document := structuredreview.Document{
			SchemaVersion: 3, Summary: "Synthetic review", Verdict: structuredreview.VerdictFail,
			Findings: []structuredreview.Finding{
				{
					Severity: severity, Kind: kind, Topic: "security", Confidence: confidence,
					Problem: problem, Fix: fix, File: "shared.go", StartLine: 8, EndLine: 8, Location: "shared.go:8",
				},
				{
					Severity: "low", Kind: "improvement", Topic: "documentation", Confidence: 1,
					Problem: "The same location also documents an incorrect invoice retention policy.",
					Fix:     "State the actual policy while preserving account recovery guidance. No runtime reproduction applies.",
					File:    "shared.go", StartLine: 8, EndLine: 8, Location: "shared.go:8",
				},
			},
		}
		before, err := json.Marshal(document)
		require.NoError(t, err)
		// Additional instructions are unrestricted text and may contain fences;
		// locate the actual member section before decoding its JSON payload.
		prompt := BuildSynthesisPrompt([]ReviewResult{{
			Agent: "test", Status: ResultDone, Structured: &document, Output: "Stale rendering: no issues found.",
		}}, neutral, instructions)
		_, member, ok := strings.Cut(prompt, "\n### Review 1\n")
		require.True(t, ok)
		_, payload, ok := strings.Cut(member, "```json\n")
		require.True(t, ok)
		payload, _, ok = strings.Cut(payload, "\n```")
		require.True(t, ok)
		decoded, err := structuredreview.Decode([]byte(payload))
		require.NoError(t, err)
		assert.Equal(document, decoded, "all structured findings take priority over stale Markdown, even at one location")
		after, err := json.Marshal(document)
		require.NoError(t, err)
		assert.Equal(before, after, "synthesis preparation must not modify caller-owned findings")
		return !t.Failed()
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

// Storage decoding is intentionally more permissive than the live boundary.
// An inability assessment must remain a provider failure even with findings;
// it must never reach Markdown as a successful review that hides those fields.
func TestBehaviorHandoffRejectsUnableLiveReviewWithFindings(t *testing.T) {
	repo := t.TempDir()
	property := func(problemText, fixText string) bool {
		document := structuredreview.Document{
			Summary: "Unable to assess the reviewed material", Verdict: structuredreview.VerdictUnableToReview,
			Findings: []structuredreview.Finding{{
				Severity: "high", Kind: "defect", Confidence: 0.7,
				Problem: "Alleged behavior: " + problemText,
				Fix:     "Proposed correction, not verified: " + fixText,
			}},
		}
		raw, err := json.Marshal(document)
		require.NoError(t, err)
		decoded, err := structuredreview.Decode(raw)
		require.NoError(t, err, "historical storage compatibility is intentional")
		require.Len(t, decoded.Findings, 1)
		a := &structuredBatchAgent{name: "test", result: raw}
		result, err := RunAgentReview(t.Context(), a, repo, "HEAD", "prompt", "default", "", nil, nil)
		require.ErrorContains(t, err, "could not review the change")
		assert.Empty(t, result.Output)
		assert.Nil(t, result.Structured)
		return !t.Failed()
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestBehaviorHandoffSurvivesSynthesisOutput(t *testing.T) {
	property := func(problemText, fixText string, thresholdIndex uint8) bool {
		assert := assert.New(t)
		problem := "The document's two retention promises conflict.\n" + problemText
		fix := "State the actual policy; preserve recovery guidance.\nNo runtime reproduction applies.\n" + fixText
		threshold := []string{"", "low", "medium", "high", "critical"}[thresholdIndex%5]
		document := structuredreview.Document{
			Summary: "Synthetic document review", Verdict: structuredreview.VerdictFail,
			Findings: []structuredreview.Finding{{
				Severity: "low", Kind: "improvement", Topic: "documentation", Confidence: 0.6,
				Problem: problem, Fix: fix, Sources: []int{1},
			}},
		}
		raw, err := json.Marshal(document)
		require.NoError(t, err)
		decoded, err := DecodeSynthesisDocument(raw, []ReviewResult{{Agent: "test"}})
		require.NoError(t, err)
		assert.Contains(decoded.Markdown(threshold), "**Problem:** "+strings.TrimSpace(problem)+"\n\n**Fix:** "+strings.TrimSpace(fix)+"\n")
		return !t.Failed()
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}
