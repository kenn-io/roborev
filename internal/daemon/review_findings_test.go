package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const findingsReviewDocument = `{"schema_version":2,"summary":"Two problems.","verdict":"fail","findings":[` +
	`{"severity":"high","problem":"Nil dereference.","fix":"Check first.","location":"a.go:10"},` +
	`{"severity":"low","problem":"Typo.","fix":"Spell it.","location":null}]}`

type reviewFindingsResponse struct {
	JobID      int64  `json:"job_id"`
	ReviewUUID string `json:"review_uuid"`
	Findings   []struct {
		ID       string `json:"id"`
		Index    int    `json:"index"`
		Severity string `json:"severity"`
		Problem  string `json:"problem"`
		Fix      string `json:"fix"`
		Location string `json:"location"`
	} `json:"findings"`
}

func TestReviewFindingsListsStableSelectors(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "findings-api", "test", findingsReviewDocument)
	path := fmt.Sprintf("/api/review/findings?job_id=%d", job.ID)

	first := serveHuma(t, server, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var got reviewFindingsResponse
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &got))

	assert := assert.New(t)
	assert.Equal(job.ID, got.JobID)
	require.Len(t, got.Findings, 2)
	assert.Equal(0, got.Findings[0].Index)
	assert.Equal("high", got.Findings[0].Severity)
	assert.Equal("Nil dereference.", got.Findings[0].Problem)
	assert.Equal("Check first.", got.Findings[0].Fix)
	assert.Equal("a.go:10", got.Findings[0].Location)
	assert.Empty(got.Findings[1].Location)
	for _, f := range got.Findings {
		id, err := findingid.Parse(f.ID)
		require.NoError(t, err)
		assert.Equal(f.Index, id.Index)
		assert.Equal(got.ReviewUUID, id.ReviewUUID.String())
		snap, err := db.FindingSnapshot(t.Context(), f.ID)
		require.NoError(t, err, "a listed selector is pinned")
		assert.Equal(f.Problem, snap.Finding.Problem)
	}

	second := serveHuma(t, server, http.MethodGet, path, nil)
	assert.JSONEq(first.Body.String(), second.Body.String(), "selectors do not change between calls")
}

func TestReviewFindingsHasNothingToListForReviewsWithoutFindings(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	clean := testutil.CreateCompletedReview(t, db, repo.ID, "findings-clean", "test",
		`{"schema_version":2,"summary":"Clean.","verdict":"pass","findings":[]}`)
	legacy := testutil.CreateCompletedReview(t, db, repo.ID, "findings-legacy", "test", findingsReviewDocument)
	_, err := db.Exec(`UPDATE reviews SET structured_output = ? WHERE job_id = ?`,
		`{"schema_version":0,"legacy":{"markdown":"Looks fine.","recorded_verdict":true}}`, legacy.ID)
	require.NoError(t, err)

	for name, jobID := range map[string]int64{"clean": clean.ID, "legacy prose": legacy.ID} {
		rr := serveHuma(t, server, http.MethodGet, fmt.Sprintf("/api/review/findings?job_id=%d", jobID), nil)
		require.Equal(t, http.StatusOK, rr.Code, name)
		var got reviewFindingsResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got), name)
		assert.Equal(t, jobID, got.JobID, name)
		assert.Empty(t, got.Findings, name)
		assert.Contains(t, rr.Body.String(), `"findings":[]`, "%s: an empty list, not null", name)
	}
}

func TestReviewFindingsRejectsBadRequests(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	queued, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "findings-queued", Agent: "test"})
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"no job":         {"/api/review/findings", http.StatusUnprocessableEntity},
		"unknown job":    {"/api/review/findings?job_id=9999", http.StatusNotFound},
		"no review yet":  {fmt.Sprintf("/api/review/findings?job_id=%d", queued.ID), http.StatusNotFound},
		"not a number":   {"/api/review/findings?job_id=abc", http.StatusUnprocessableEntity},
		"zero is no job": {"/api/review/findings?job_id=0", http.StatusUnprocessableEntity},
	} {
		rr := serveHuma(t, server, http.MethodGet, tc.path, nil)
		assert.Equal(t, tc.want, rr.Code, "%s: %s", name, rr.Body.String())
	}
}
