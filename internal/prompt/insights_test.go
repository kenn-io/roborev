package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestBuildInsightsPrompt_Basic(t *testing.T) {
	t.Parallel()

	now := time.Now()
	finished := now.Add(-1 * time.Hour)
	data := InsightsData{
		Reviews: []InsightsReview{
			{
				JobID:      1,
				Agent:      "codex",
				GitRef:     "abc1234",
				Branch:     "main",
				FinishedAt: &finished,
				Output:     "Found SQL injection in handler.go:42",
				Closed:     false,
				Verdict:    "F",
			},
			{
				JobID:      2,
				Agent:      "gemini",
				GitRef:     "def5678",
				FinishedAt: &finished,
				Output:     "Missing error handling in parser.go:10",
				Closed:     true,
				Verdict:    "F",
			},
		},
		Guidelines: "Always check error returns",
		RepoName:   "myrepo",
		Since:      now.Add(-30 * 24 * time.Hour),
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "code review insights analyst")
	assert.Contains(t, result, "Always check error returns")
	assert.Contains(t, result, "SQL injection")
	assert.Contains(t, result, "Missing error handling")
	assert.Contains(t, result, "unaddressed")
	assert.Contains(t, result, "addressed/closed")
	assert.Contains(t, result, "codex")
}

func TestBuildInsightsPrompt_PanelMetadata(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		jobType string
		role    string
		panel   string
	}{
		{"panel", storage.JobTypeSynthesis, storage.PanelRoleSynthesis, "careful"},
		{"standalone", storage.JobTypeReview, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := InsightsReviewFromJob(storage.ReviewJob{
				ID: 1, Agent: "test", JobType: tc.jobType, PanelRole: tc.role,
				PanelName: tc.panel, PanelSummary: &storage.PanelSummary{MembersTotal: 3},
			}, "Review finding", nil, false)
			result := BuildInsightsPrompt(InsightsData{Reviews: []InsightsReview{review}})
			assert.Contains(t, result, "Review finding")
			if tc.panel != "" {
				assert.Contains(t, result, "**Panel:** careful (3 configured members)")
			} else {
				assert.NotContains(t, result, "**Panel:**")
			}
		})
	}
}

func TestBuildInsightsPrompt_IncludesComments(t *testing.T) {
	t.Parallel()

	data := InsightsData{
		Reviews: []InsightsReview{
			{
				JobID:  1,
				Agent:  "test",
				Output: "finding",
				Responses: []storage.Response{
					{Responder: "user", Response: "This was intentional"},
					{Responder: "agent", Response: "Confirmed after discussion"},
				},
			},
		},
		Since: time.Now().Add(-7 * 24 * time.Hour),
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "Comments on this review:")
	assert.Contains(t, result, `- user: "This was intentional"`)
	assert.Contains(t, result, `- agent: "Confirmed after discussion"`)
}

func TestBuildInsightsPrompt_TruncatesLargeCommentThread(t *testing.T) {
	t.Parallel()

	var responses []storage.Response
	for i := range 50 {
		responses = append(responses, storage.Response{
			Responder: "user",
			Response:  strings.Repeat("comment text ", 20) + fmt.Sprintf("comment-%d", i),
		})
	}

	data := InsightsData{
		Reviews: []InsightsReview{
			{
				JobID:     1,
				Agent:     "test",
				Output:    "a finding",
				Responses: responses,
			},
		},
		Since:         time.Now().Add(-7 * 24 * time.Hour),
		MaxPromptSize: 50000,
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "a finding")
	assert.Contains(t, result, "Comments on this review:")
	assert.Contains(t, result, "remaining comments truncated")
	// The review should still be included despite large comments
	assert.Contains(t, result, "Review 1")
}

func TestBuildInsightsPrompt_NoGuidelines(t *testing.T) {
	t.Parallel()

	data := InsightsData{
		Reviews: []InsightsReview{
			{JobID: 1, Agent: "test", Output: "finding"},
		},
		Since: time.Now().Add(-7 * 24 * time.Hour),
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "No review guidelines configured")
}

func TestBuildInsightsPrompt_PrioritizesOpen(t *testing.T) {
	t.Parallel()

	data := InsightsData{
		MaxReviews: 2,
		Reviews: []InsightsReview{
			{JobID: 1, Agent: "test", Output: "open finding 1", Closed: false},
			{JobID: 2, Agent: "test", Output: "open finding 2", Closed: false},
			{JobID: 3, Agent: "test", Output: "open finding 3", Closed: false},
			{JobID: 4, Agent: "test", Output: "closed finding", Closed: true},
		},
		Since: time.Now().Add(-7 * 24 * time.Hour),
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "open finding 1")
	assert.Contains(t, result, "open finding 2")
	assert.NotContains(t, result, "closed finding")
	assert.Contains(t, result, "additional failing reviews were omitted")
}

func TestBuildInsightsPrompt_TruncatesLongOutput(t *testing.T) {
	t.Parallel()

	longOutput := strings.Repeat("x", 10000)
	data := InsightsData{
		Reviews: []InsightsReview{
			{JobID: 1, Agent: "test", Output: longOutput},
		},
		MaxOutputPerReview: 100,
		Since:              time.Now().Add(-7 * 24 * time.Hour),
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "... (truncated)")
	assert.NotContains(t, result, longOutput)
}

func TestBuildInsightsPrompt_Empty(t *testing.T) {
	t.Parallel()

	data := InsightsData{
		Since: time.Now().Add(-7 * 24 * time.Hour),
	}

	result := BuildInsightsPrompt(data)

	assert.Contains(t, result, "0 failing review(s)")
}

func TestBuildInsightsPrompt_RespectsPromptBudget(t *testing.T) {
	t.Parallel()

	var reviews []InsightsReview
	for i := range 20 {
		reviews = append(reviews, InsightsReview{
			JobID:  int64(i + 1),
			Agent:  "test",
			Output: strings.Repeat("finding text ", 100),
		})
	}

	budget := 5000
	data := InsightsData{
		Reviews:       reviews,
		Since:         time.Now().Add(-7 * 24 * time.Hour),
		MaxPromptSize: budget,
	}

	result := BuildInsightsPrompt(data)

	assert.LessOrEqual(t, len(result), budget)
	assert.Contains(t, result, "omitted due to prompt size limits")
}

func TestBuildInsightsPrompt_TruncatesLargeGuidelines(t *testing.T) {
	t.Parallel()

	hugeGuidelines := strings.Repeat("guideline rule ", 10000)
	data := InsightsData{
		Reviews: []InsightsReview{
			{JobID: 1, Agent: "test", Output: "a finding"},
		},
		Guidelines:    hugeGuidelines,
		Since:         time.Now().Add(-7 * 24 * time.Hour),
		MaxPromptSize: 10000,
	}

	result := BuildInsightsPrompt(data)

	assert.LessOrEqual(t, len(result), data.MaxPromptSize)
	assert.Contains(t, result, "guidelines truncated")
	assert.Contains(t, result, "a finding")
}

// A stale review-version match or budgeting new metadata would corrupt evidence.
func TestBuildInsightsPrompt_MeasuredEvidenceAndCoverage(t *testing.T) {
	stats := &storage.FindingStats{
		LogicalReviews: 9, StructuredReviews: 7,
		Total:            storage.FindingOutcomeCounts{Findings: 12, Labeled: 5, Positive: 3, Negative: 2},
		UnavailableTiers: []string{"K", "D", "panel R"},
		Evidence: []storage.FindingEvidence{
			{JobID: 1, ReviewUUID: "current", Outcome: "confirmed", Tier: "R", Explanation: strings.Repeat("complete evidence ", 500)},
			{JobID: 2, ReviewUUID: "old", Outcome: "refuted", Tier: "R", Explanation: "STALE ASSESSMENT"},
		},
	}
	result := BuildInsightsPrompt(InsightsData{
		Measured: stats, MaxPromptSize: 5000, MaxReviews: 1, MaxOutputPerReview: 10,
		Reviews: []InsightsReview{
			{JobID: 1, ReviewUUID: "current", Output: "long review finding"},
			{JobID: 2, ReviewUUID: "replacement", Output: "other finding"},
		},
	})
	assert := assert.New(t)
	assert.Less(strings.Index(result, "## Measured outcomes"), strings.Index(result, "## Failing Reviews"))
	assert.Contains(result, `"logical_reviews": 9`)
	assert.Contains(result, `"positive": 3`)
	assert.Contains(result, `"Outcome": "confirmed"`)
	assert.Contains(result, strings.Repeat("complete evidence ", 500))
	assert.NotContains(result, "STALE ASSESSMENT")
	assert.Contains(result, `"Closed": false`)
	assert.Contains(result, "Version-matched evidence coverage: 1/12 full-window findings.")
	assert.Contains(result, "Fetched failing reviews: 2; included excerpts: 1; truncated excerpts: 1")
	assert.Contains(result, "none found in supplied evidence")
}

func TestBuildInsightsPrompt_PriorUsesCompleteGuidelines(t *testing.T) {
	full := strings.Repeat("existing rule\n", 200) + "Keep exact prior line."
	result := BuildInsightsPrompt(InsightsData{
		Guidelines: full, MaxPromptSize: 5000,
		Prior: &InsightsPrior{JobID: 8, Suggestions: "Keep exact prior line."},
	})
	assert.Contains(t, result, full)
	assert.Contains(t, result, "insufficient comparable evidence")
	assert.Contains(t, result, "within noise")
	assert.Contains(t, result, "No guideline change recommended")
}

func TestRecoverInsightsContext(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"section", "## 4. Guideline Gaps\nGap\n## 5. Suggested Guideline Additions\n\nCheck errors.\n## 6. Other\nignored", "Check errors."},
		{"missing", "Only prose", ""},
		{"empty", "## 5. Suggested Guideline Additions\n\n", ""},
		{"duplicate", "## 5. Suggested Guideline Additions\nA\n## 5. Suggested Guideline Additions\nB", ""},
		{"quoted fence", "```markdown\n## 5. Suggested Guideline Additions\nFake\n```", ""},
		{"unclosed fence", "```\n## 5. Suggested Guideline Additions\nFake", ""},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, RecoverInsightsSuggestions(tc.output)) })
	}
	baseline := RecoverInsightsBaseline("<!-- roborev-insights-measured-v1 -->\n{\"logical_reviews\":7,\"structured_reviews\":7,\"total\":{\"findings\":0,\"labeled\":0,\"positive\":0,\"negative\":0,\"excluded\":0,\"unlabeled\":0},\"time_basis\":\"finished_at\",\"since\":\"2026-01-01T00:00:00Z\",\"as_of\":\"2026-02-01T00:00:00Z\"}\n<!-- /roborev-insights-measured-v1 -->")
	require.NotNil(t, baseline)
	assert.Equal(t, 7, baseline.LogicalReviews)
	assert.Nil(t, RecoverInsightsBaseline("<!-- roborev-insights-measured-v1 -->\n{broken}\n<!-- /roborev-insights-measured-v1 -->"))
	assert.Nil(t, RecoverInsightsBaseline("<!-- roborev-insights-measured-v1 -->\n{}\n<!-- /roborev-insights-measured-v1 -->"))
}

func TestRecoverInsightsBaselineRequiresCounts(t *testing.T) {
	// A date without measured denominators must not become a zero-count baseline.
	assert.Nil(t, RecoverInsightsBaseline("<!-- roborev-insights-measured-v1 -->\n{\"time_basis\":\"finished_at\",\"since\":\"2026-01-01T00:00:00Z\",\"as_of\":\"2026-01-02T00:00:00Z\"}\n<!-- /roborev-insights-measured-v1 -->"))
}

func FuzzRecoverInsightsSuggestions(f *testing.F) {
	f.Add("## 5. Suggested Guideline Additions\n\nCheck errors.")
	f.Add("```markdown\n## 5. Suggested Guideline Additions\nFake\n```")
	f.Fuzz(func(t *testing.T, output string) {
		got := RecoverInsightsSuggestions(output)
		assert.Equal(t, strings.TrimSpace(got), got)
		if got != "" {
			assert.Contains(t, output, got)
		}
		// Single-line suggestion text must survive recovery, including arbitrary
		// Unicode, whitespace and punctuation, without promoting it to a heading.
		line := strings.NewReplacer("\n", " ", "\r", " ").Replace(output)
		want := strings.TrimSpace("suggestion " + line)
		assert.Equal(t, want, RecoverInsightsSuggestions("## 5. Suggested Guideline Additions\n\n"+want))
	})
}

func TestGoldenPrompt_InsightsMeasuredPrior(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	stats := &storage.FindingStats{
		Since: start, AsOf: end, RepoPath: "/repo", Branch: "main", TimeBasis: "finished_at", GroupBy: "severity", MinSamples: 5,
		LogicalReviews: 3, StructuredReviews: 2, LegacyDocuments: 1,
		Total:            storage.FindingOutcomeCounts{Findings: 2, Labeled: 1, Positive: 1, Unlabeled: 1, RateState: "below_minimum", FailingReviews: 2, ClosedReviews: 1},
		UnavailableTiers: []string{"K", "D", "panel R"}, TierLabeled: map[string]int{"R": 1},
		Evidence: []storage.FindingEvidence{{JobID: 2, ReviewUUID: "version-2", Index: 0, Outcome: "confirmed", Tier: "R", AssessmentJobID: 3, AssessmentUUID: "assessment-3", Explanation: "The error path is reachable."}},
	}
	result := BuildInsightsPrompt(InsightsData{
		Measured: stats, Guidelines: "Check returned errors.", Since: start,
		Reviews: []InsightsReview{{JobID: 2, ReviewUUID: "version-2", Agent: "test", GitRef: "commit", FinishedAt: &end, Output: "Missing error handling.", Closed: true}},
		Prior:   &InsightsPrior{JobID: 1, CompletedAt: start, Suggestions: "Check returned errors."},
	})
	assertGolden(t, strings.TrimRight(result, "\n")+"\n", "insights_measured_prior.golden")
}

func FuzzRecoverInsightsBaseline(f *testing.F) {
	f.Add("not measured context")
	f.Add(BuildInsightsPrompt(InsightsData{Measured: &storage.FindingStats{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), AsOf: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), TimeBasis: "finished_at"}}))
	f.Add("<!-- roborev-insights-measured-v1 -->\n{\"time_basis\":\"finished_at\",\"since\":\"2026-01-01T00:00:00Z\",\"as_of\":\"2026-01-02T00:00:00Z\"}\n<!-- /roborev-insights-measured-v1 -->")
	f.Add("<!-- roborev-insights-measured-v1 -->\n{}\n<!-- /roborev-insights-measured-v1 -->")
	f.Fuzz(func(t *testing.T, input string) {
		baseline := RecoverInsightsBaseline(input)
		if baseline != nil {
			assert.Equal(t, "finished_at", baseline.TimeBasis)
			assert.False(t, baseline.Since.IsZero())
			assert.False(t, baseline.AsOf.IsZero())
			assert.False(t, baseline.AsOf.Before(baseline.Since))
		}
	})
}

func TestBuildInsightsPrompt_PriorBaseline(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	result := BuildInsightsPrompt(InsightsData{Prior: &InsightsPrior{Suggestions: "Check errors.", Baseline: &storage.FindingStats{Since: start, AsOf: start.Add(24 * time.Hour), LogicalReviews: 7}}})
	assert.Contains(t, result, `"logical_reviews": 7`)
	assert.Contains(t, result, `"since": "2026-01-01T00:00:00Z"`)
	assert.Contains(t, result, "Suggestion-specific before/after pattern counts are unavailable")
	assert.Contains(t, result, "insufficient comparable evidence")
}
