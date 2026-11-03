package mcpserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestReviewDecisions(t *testing.T) {
	run := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	comment := func(at time.Time, text string) storage.Response {
		return storage.Response{Responder: "roborev-fix", Response: text, CreatedAt: at}
	}
	review := BranchReview{
		Review: storage.Review{CreatedAt: run},
		Comments: []storage.Response{
			comment(run.Add(-time.Minute), "- high a.go:3: Decline: not worth it."),
			comment(run.Add(time.Minute), "- high `a.go:3`: Fix: guarded it.\n- low b.go: Ask: is a test wanted?"),
			comment(run.Add(2*time.Minute), "Triage notes.\n- medium c.go: Invalid: unreachable."),
			comment(run.Add(3*time.Minute), "Thanks, looks good."),
		},
		Assessments: []FindingAssessment{
			{Finding: "trusts its input", Outcome: "refuted", Explanation: "Callers check it."},
			{Finding: "old wording", Outcome: "confirmed", Explanation: "Still wrong.", BeforeRerun: true},
		},
	}

	decisions, comments := reviewDecisions(review)

	assert.Equal(t, []reviewerDecision{
		{Kind: "decline", Target: "high a.go:3", Reason: "not worth it.", Source: DecisionSourceFixComment, By: "roborev-fix", BeforeRerun: true},
		{Kind: "fix", Target: "high a.go:3", Reason: "guarded it.", Source: DecisionSourceFixComment, By: "roborev-fix"},
		{Kind: "ask", Target: "low b.go", Reason: "is a test wanted?", Source: DecisionSourceFixComment, By: "roborev-fix"},
		{Kind: "invalid", Target: "medium c.go", Reason: "unreachable.", Source: DecisionSourceFixComment, By: "roborev-fix"},
		{Kind: "refuted", Target: "trusts its input", Reason: "Callers check it.", Source: DecisionSourceRebuttal},
		{Kind: "confirmed", Target: "old wording", Reason: "Still wrong.", Source: DecisionSourceRebuttal, BeforeRerun: true},
	}, decisions)
	var kept []string
	for _, c := range comments {
		kept = append(kept, c.Response)
	}
	assert.Equal(t, []string{"Triage notes.\n- medium c.go: Invalid: unreachable.", "Thanks, looks good."}, kept,
		"comments with free text are kept whole; comments that are only dispositions are not repeated")
}
