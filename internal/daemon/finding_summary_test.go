package daemon

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestHumaFindingSummaryOptions(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateCompletedReview(t, db, repo.ID, "finding-sha", "test", "No issues found.")
	plain := serveHuma(t, srv, http.MethodGet, "/api/summary", nil)
	require.Equal(t, http.StatusOK, plain.Code)
	assert.NotContains(t, plain.Body.String(), `"findings"`)
	for _, query := range []string{"by=agent", "min_samples=5", "findings=true&by=unknown", "findings=true&min_samples=-1", "findings=true&min_samples=0", "findings=true&min_samples=bad"} {
		rr := serveHuma(t, srv, http.MethodGet, "/api/summary?"+query, nil)
		assert.Equal(t, http.StatusBadRequest, rr.Code, query)
	}
	rr := serveHuma(t, srv, http.MethodGet, "/api/summary?findings=true&by=agent&min_samples=1", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var s storage.Summary
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &s))
	require.NotNil(t, s.Findings)
	assert.Equal(t, "agent", s.Findings.GroupBy)
	assert.Equal(t, 1, s.Findings.LogicalReviews)
}

func TestFindingSummarySynthesisShortcutsKeepMemberSpendOnce(t *testing.T) {
	for _, passthrough := range []bool{true, false} {
		t.Run(map[bool]string{true: "passthrough", false: "all-clean"}[passthrough], func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			run, members, _ := enqueuePanelRun(t, tc, "cost-panel", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
			completeMember(t, tc, members[0].ID, "test", "No issues found.")
			if passthrough {
				failMember(t, tc, members[1].ID)
			} else {
				completeMember(t, tc, members[1].ID, "test", "No findings to report.")
			}
			for i, member := range members {
				usage := `{"has_cost":true,"cost_usd":0}`
				if i == 1 {
					usage = `{"has_cost":true}`
				}
				_, err := tc.DB.Exec("UPDATE review_jobs SET started_at=enqueued_at,agent_invoked=1,token_usage=? WHERE id=?", usage, member.ID)
				require.NoError(t, err)
			}
			synth := releaseAndClaimSynthesis(t, tc, run)
			tc.Pool.processSynthesisJob(t.Context(), testWorkerID, synth)
			parent, err := tc.DB.GetJobByID(synth.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusDone, parent.Status)
			assert.Empty(parent.TokenUsage, "shortcut completion must not copy member usage into the no-agent parent")
			assert.False(jobAgentInvoked(t, tc, synth.ID))
			stats, err := tc.DB.GetFindingStats(storage.SummaryOptions{}, "agent")
			require.NoError(t, err)
			assert.Equal(1, stats.LogicalReviews)
			require.Len(t, stats.Rows, 1)
			assert.Equal(2, stats.Cost.JobsTotal)
			assert.Equal(1, stats.Cost.JobsWithCost)
			assert.Zero(stats.Cost.TotalUSD)
			assert.False(stats.Cost.Complete)
			assert.Equal(stats.Cost, stats.Rows[0].Cost)
			memberStats, err := tc.DB.GetFindingStats(storage.SummaryOptions{}, "member")
			require.NoError(t, err)
			require.NotNil(t, memberStats.Members)
			assert.Zero(memberStats.Members.SkippedPanelRuns)
			require.Len(t, memberStats.Members.Rows, 2)
			assert.Equal(stats.Cost, memberStats.Members.Cost)
			rows, err := tc.DB.GetPanelMemberReviews(run)
			require.NoError(t, err)
			usable := filterSucceeded(toReviewResults(rows))
			if passthrough {
				assert.Len(usable, 1)
			} else {
				assert.Len(usable, 2)
			}
			expectedLabels := reviewpkg.SynthesisSourceLabels(usable)
			review, err := tc.DB.GetReviewByJobID(synth.ID)
			require.NoError(t, err)
			raw, err := json.Marshal(review.StructuredOutput)
			require.NoError(t, err)
			document, err := structuredreview.Decode(raw)
			require.NoError(t, err)
			if passthrough {
				assert.Equal(expectedLabels, document.SourceLabels)
			} else {
				assert.Empty(document.SourceLabels)
			}
		})
	}
}

func TestHumaMemberFindingSummary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := serveHuma(t, srv, http.MethodGet, "/api/summary?findings=true&by=member", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var summary storage.Summary
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &summary))
	require.NotNil(t, summary.Findings)
	require.NotNil(t, summary.Findings.Members)
	assert.Empty(t, summary.Findings.Members.Rows)
}

func TestMemberFindingSummarySynthesisPositionParity(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	run, members, _ := enqueuePanelRun(t, tc, "source-panel", []memberSpec{
		{name: "failed", agent: "test"},
		{name: "first", agent: "first-agent"},
		{name: "second", agent: "second-agent"},
		{name: "observer", agent: "test", nonVoting: true},
	})
	failMember(t, tc, members[0].ID)
	for _, member := range members[1:] {
		completeMember(t, tc, member.ID, member.Agent, "High: member finding")
	}
	for i, reviewType := range []string{"security", "performance"} {
		_, err := tc.DB.Exec("UPDATE review_jobs SET review_type = ? WHERE id = ?", reviewType, members[i+1].ID)
		require.NoError(t, err)
	}
	rows, err := tc.DB.GetPanelMemberReviews(run)
	require.NoError(t, err)
	usable := filterSucceeded(toReviewResults(rows))
	require.Len(t, usable, 2)
	assert.Equal(t, []string{"first-agent (security)", "second-agent (performance)"}, reviewpkg.SynthesisSourceLabels(usable))
	doc, err := reviewpkg.DecodeSynthesisDocument([]byte(`{"schema_version":2,"summary":"combined","verdict":"fail","findings":[{"severity":"high","problem":"shared","fix":"repair","location":null,"sources":[1,2]},{"severity":"low","problem":"solo","fix":"repair","location":null,"sources":[2,2]}]}`), usable)
	require.NoError(t, err)
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	synth := releaseAndClaimSynthesis(t, tc, run)
	require.NoError(t, tc.DB.CompleteJobResult(synth.ID, "test", storage.ReviewCompletion{Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}))
	stats, err := tc.DB.GetFindingStats(storage.SummaryOptions{}, "member")
	require.NoError(t, err)
	require.NotNil(t, stats.Members)
	require.Len(t, stats.Members.Rows, 3)
	a := assert.New(t)
	a.Zero(stats.Members.SkippedPanelRuns)
	failed, first, second := stats.Members.Rows[0], stats.Members.Rows[1], stats.Members.Rows[2]
	a.Equal("failed", failed.MemberName)
	a.Equal(1, failed.Runs)
	a.Zero(failed.Cited)
	a.Equal("first", first.MemberName)
	a.Equal(1, first.Cited)
	a.Equal(1, first.Agreed)
	a.Equal("second", second.MemberName)
	a.Equal(2, second.Cited)
	a.Equal(1, second.Agreed)
	a.Equal(1, second.Solo)
}
