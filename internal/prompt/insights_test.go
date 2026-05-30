package prompt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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
