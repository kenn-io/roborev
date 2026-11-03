package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func decisionFixture(findings ...structuredreview.Finding) (structuredreview.Document, []reviewerFinding, string) {
	doc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion, Summary: "Review summary mentions a.go.",
		Verdict: structuredreview.VerdictFail, Findings: findings,
	}
	rows := make([]reviewerFinding, len(findings))
	for i := range findings {
		rows[i].Index = i + 1
	}
	return doc, rows, doc.Markdown("")
}

func jobComment(text string) storage.Response {
	jobID := int64(7)
	return storage.Response{JobID: &jobID, Responder: "roborev-fix", Response: text}
}

func kinds(f reviewerFinding) []string {
	var out []string
	for _, d := range f.Decisions {
		out = append(out, d.Kind)
	}
	return out
}

func TestAttachDecisionsMatchesFixLines(t *testing.T) {
	high := func(file string, line int, problem string) structuredreview.Finding {
		return structuredreview.Finding{Severity: "high", Problem: problem, Fix: "Fix it.", File: file, StartLine: line, EndLine: line}
	}

	t.Run("line numbers keep repeated lists aligned", func(t *testing.T) {
		doc, rows, _ := decisionFixture(high("a.go", 3, "First."), high("a.go", 9, "Second."))
		unmatched, comments := attachDecisions(rows, doc, BranchReview{Comments: []storage.Response{
			jobComment("- high a.go:9: Decline: not worth it.\n- high a.go:3: Fix: done."),
			jobComment("- high a.go:3: Invalid: stale.\n- high a.go:9: Ask: unclear."),
		}})
		assert.Empty(t, unmatched)
		assert.Empty(t, comments)
		assert.Equal(t, []string{"fix", "invalid"}, kinds(rows[0]))
		assert.Equal(t, []string{"decline", "ask"}, kinds(rows[1]))
	})

	t.Run("ambiguous lines stay on the review with their target", func(t *testing.T) {
		doc, rows, _ := decisionFixture(high("a.go", 0, "First."), high("a.go", 0, "Second."))
		unmatched, _ := attachDecisions(rows, doc, BranchReview{Comments: []storage.Response{
			jobComment("- high a.go: Fix: done."),
		}})
		assert.Empty(t, rows[0].Decisions)
		assert.Empty(t, rows[1].Decisions)
		assert.Equal(t, []reviewerDecision{{
			Kind: "fix", Reason: "done.", Source: DecisionSourceFixComment, By: "roborev-fix", Target: "high a.go",
		}}, unmatched)
	})

	t.Run("commit comments never attach to one review's findings", func(t *testing.T) {
		doc, rows, _ := decisionFixture(high("a.go", 3, "First."))
		commitID := int64(3)
		unmatched, comments := attachDecisions(rows, doc, BranchReview{Comments: []storage.Response{
			{CommitID: &commitID, Responder: "developer", Response: "- high a.go:3: Decline: covered elsewhere."},
		}})
		assert.Empty(t, rows[0].Decisions)
		assert.Empty(t, comments)
		assert.Equal(t, "high a.go:3", unmatched[0].Target)
	})

	t.Run("free text keeps the comment", func(t *testing.T) {
		doc, rows, _ := decisionFixture(high("a.go", 3, "First."))
		_, comments := attachDecisions(rows, doc, BranchReview{Comments: []storage.Response{
			jobComment("Triage notes.\n- high a.go:3: Fix: done."),
		}})
		assert.Equal(t, []string{"fix"}, kinds(rows[0]))
		assert.Len(t, comments, 1)
	})
}

func TestAttachDecisionsMatchesRebuttalExcerpts(t *testing.T) {
	doc, _, output := decisionFixture(
		structuredreview.Finding{Severity: "medium", Problem: "Avg trusts len.", Fix: "Check a.go input.", File: "a.go", StartLine: 2},
		structuredreview.Finding{Severity: "low", Problem: "Sum can overflow.", Fix: "Check a.go input.", File: "b.go"},
	)
	for name, tc := range map[string]struct {
		excerpt string
		want    int // 0-based finding, or -1 for the review
	}{
		"problem text":                {"Avg trusts len.", 0},
		"rendered block with labels":  {"**Problem:** Sum can overflow.", 1},
		"text in two findings":        {"Check a.go input.", -1},
		"summary text":                {"Review summary mentions", -1},
		"spans two findings":          {"Check a.go input. ### 2. Low", -1},
		"excerpt with extra newlines": {"Avg   trusts\nlen.", 0},
	} {
		t.Run(name, func(t *testing.T) {
			rows := []reviewerFinding{{Index: 1}, {Index: 2}}
			review := BranchReview{
				Review:      storage.Review{Output: output},
				Assessments: []FindingAssessment{{Finding: tc.excerpt, Outcome: "refuted", Explanation: "Wrong."}},
			}
			unmatched, _ := attachDecisions(rows, doc, review)
			if tc.want < 0 {
				assert.Equal(t, tc.excerpt, unmatched[0].Target)
				assert.Empty(t, rows[0].Decisions)
				assert.Empty(t, rows[1].Decisions)
				return
			}
			assert.Empty(t, unmatched)
			assert.Equal(t, []string{"refuted"}, kinds(rows[tc.want]))
		})
	}
}
