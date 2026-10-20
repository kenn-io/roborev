package review

import (
	"context"
	"encoding/json/jsontext"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func located(file string, start, end int) structuredreview.Finding {
	finding := structuredreview.Finding{
		Severity: "medium", Kind: structuredreview.KindDefect, Confidence: 0.5,
		Problem: "Problem in " + file, Fix: "Fix it.",
	}
	finding.SetLocation(file, start, end)
	return finding
}

func numberedLines(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("\n")
	}
	return b.String()
}

func TestSettleFindingsChecksLocationsAgainstTheReviewedCommit(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.CommitFile("pkg/a.go", numberedLines(10), "add a.go")
	// The working tree differs from the commit, so a range that survives
	// only fits the committed file.
	repo.WriteFile("pkg/a.go", numberedLines(3))

	doc := structuredreview.Document{Findings: []structuredreview.Finding{
		located("b/pkg/a.go", 4, 6),
		located(filepath.Join(repo.Path(), "pkg", "a.go"), 8, 40),
		located("./pkg/a.go", 50, 55),
		located("missing.go", 2, 2),
		located("../outside.go", 1, 1),
	}}
	settleFindings(context.Background(), repo.Path(), sha, &doc)

	require.Len(t, doc.Findings, 5)
	got := make(map[string]string)
	for _, finding := range doc.Findings {
		got[strings.TrimPrefix(finding.Problem, "Problem in ")] = finding.Location
	}
	assert := assert.New(t)
	assert.Equal("pkg/a.go:4-6", got["b/pkg/a.go"], "diff prefixes are removed")
	assert.Equal("pkg/a.go:8-10", got[filepath.Join(repo.Path(), "pkg", "a.go")], "end line is clamped to the file")
	assert.Equal("pkg/a.go", got["./pkg/a.go"], "a range past the end of the file is dropped")
	assert.Empty(got["missing.go"], "a missing file loses its location")
	assert.Empty(got["../outside.go"], "a path outside the repository loses its location")
}

func TestSettleFindingsUsesTheEndOfARange(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.HeadSHA()
	head := repo.CommitFile("added.go", numberedLines(5), "add file")

	doc := structuredreview.Document{Findings: []structuredreview.Finding{located("added.go", 2, 3)}}
	settleFindings(context.Background(), repo.Path(), base+".."+head, &doc)

	assert.Equal(t, "added.go:2-3", doc.Findings[0].Location)
}

func TestSettleFindingsReadsTheWorkingTreeForUncommittedChanges(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	repo.WriteFile("new.go", numberedLines(4))

	doc := structuredreview.Document{Findings: []structuredreview.Finding{located("new.go", 3, 9)}}
	settleFindings(context.Background(), repo.Path(), "dirty", &doc)

	assert.Equal(t, "new.go:3-4", doc.Findings[0].Location)
}

func TestSettleFindingsLeavesLocationsWhenTheCheckoutIsMissing(t *testing.T) {
	doc := structuredreview.Document{Findings: []structuredreview.Finding{located("main.go", 3, 4)}}
	settleFindings(context.Background(), filepath.Join(t.TempDir(), "gone"), "dirty", &doc)

	assert.Equal(t, "main.go:3-4", doc.Findings[0].Location)
}

func TestTidyFindingsMergesTheSameProblemAtTheSamePlace(t *testing.T) {
	first := located("a.go", 10, 12)
	first.Problem = "The retry loop never stops when the server returns 503."
	first.Severity = "medium"
	first.Sources = []int{2}
	second := located("a.go", 13, 13)
	second.Problem = "Retry loop never stops when the server returns a 503 response."
	second.Severity = "high"
	second.Sources = []int{1}
	otherProblem := located("a.go", 10, 12)
	otherProblem.Problem = "The new timeout setting is parsed and then ignored."
	otherFile := located("b.go", 10, 12)
	otherFile.Problem = first.Problem
	unlocated := located("", 0, 0)
	unlocated.Problem = first.Problem
	unlocatedTwin := unlocated

	got := tidyFindings([]structuredreview.Finding{first, second, otherProblem, otherFile, unlocated, unlocatedTwin})

	require.Len(t, got, 5, "only the two retry findings in a.go merge")
	assert := assert.New(t)
	assert.Equal("high", got[0].Severity, "the more severe copy is kept")
	assert.Equal(second.Problem, got[0].Problem)
	assert.Equal([]int{1, 2}, got[0].Sources, "a merged finding keeps every source")
}

func TestTidyFindingsRanksBySeverityThenSupportThenConfidence(t *testing.T) {
	finding := func(problem, severity string, sources int, confidence float64) structuredreview.Finding {
		f := located(problem+".go", 1, 1)
		f.Problem, f.Severity, f.Confidence = problem, severity, confidence
		for i := range sources {
			f.Sources = append(f.Sources, i+1)
		}
		return f
	}
	got := tidyFindings([]structuredreview.Finding{
		finding("low", "low", 3, 1),
		finding("medium-sure", "medium", 1, 0.9),
		finding("medium-shared", "medium", 2, 0.2),
		finding("critical", "critical", 0, 0.1),
		finding("medium-unsure", "medium", 1, 0.3),
	})

	order := make([]string, len(got))
	for i, f := range got {
		order[i] = f.Problem
	}
	assert.Equal(t, []string{"critical", "medium-shared", "medium-sure", "medium-unsure", "low"}, order)
}

func TestRunAgentReviewStoresSettledFindings(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	a := &structuredBatchAgent{
		name: "structured",
		result: jsontext.Value(`{
  "schema_version": 3,
  "verdict": "fail",
  "summary": "Two problems.",
  "findings": [
    {"severity":"low","kind":"improvement","topic":"documentation","confidence":0.6,"problem":"The comment on main describes the old greeting.","fix":"Update it.","file":"b/main.go","start_line":3,"end_line":99},
    {"severity":"high","kind":"defect","confidence":0.9,"problem":"main prints to stderr.","fix":"Print to stdout.","file":"ghost.go","start_line":1,"end_line":1}
  ]
}`),
	}

	got, err := RunAgentReview(context.Background(), a, repo.Path(), repo.HeadSHA(), "prompt", "", "low", nil)
	require.NoError(t, err)

	stored, err := structuredreview.Decode(got.StructuredOutput)
	require.NoError(t, err)
	require.Len(t, stored.Findings, 2)
	assert := assert.New(t)
	assert.Equal("high", stored.Findings[0].Severity, "findings are ranked before storage")
	assert.Empty(stored.Findings[0].Location, "a missing file is not stored")
	assert.Equal("main.go:3-5", stored.Findings[1].Location, "the stored location matches the reviewed file")
	assert.Equal(structuredreview.KindImprovement, stored.Findings[1].Kind)
	assert.Equal(structuredreview.TopicDocumentation, stored.Findings[1].Topic)
	assert.Contains(got.Output, "### 2. Low\n")
	assert.Contains(got.Output, "**Location:** main.go:3-5")
}

func TestFormatCommentLeavesKindAndTopicOut(t *testing.T) {
	defect := located("a.go", 3, 3)
	defect.Topic, defect.Problem = structuredreview.TopicSecurity, "Nil map write."
	test := located("a_test.go", 0, 0)
	test.Severity, test.Kind, test.Topic, test.Problem = "low", structuredreview.KindImprovement, structuredreview.TopicTests, "No test covers the empty input."
	doc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion, Summary: "s",
		Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{defect, test},
	}

	got := FormatComment(PrepareComment(CommentConfig{}, ReviewResult{Structured: &doc}))

	assert := assert.New(t)
	assert.Contains(got, "- `a.go:3`: Nil map write.")
	assert.Contains(got, "- `a_test.go`: No test covers the empty input.")
	for _, word := range []string{"ecurity", "mprovement", "efect"} {
		assert.NotContains(got, word, "PR comments must not carry a finding's kind or topic")
	}
}
