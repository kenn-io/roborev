package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type sortedJobsPage struct {
	Jobs       []storage.ReviewJob `json:"jobs"`
	HasMore    bool                `json:"has_more"`
	NextCursor *string             `json:"next_cursor"`
}

func fetchSortedJobsPage(t *testing.T, srv *Server, query string) sortedJobsPage {
	t.Helper()
	rr := serveHuma(t, srv, http.MethodGet, "/api/jobs?"+query, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var page sortedJobsPage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
	return page
}

// seedCostJobs creates five jobs with costs 0.3, none, 0.3, 1.25, none and
// returns their IDs in creation order.
func seedCostJobs(t *testing.T, db *storage.DB) []int64 {
	t.Helper()
	repo := testutil.CreateTestRepo(t, db)
	jobs := testutil.CreateTestJobs(t, db, repo, 5, "test-agent")
	ids := make([]int64, len(jobs))
	for i, usage := range []any{
		`{"has_cost":true,"cost_usd":0.3}`, nil, `{"has_cost":true,"cost_usd":0.3}`,
		`{"has_cost":true,"cost_usd":1.25}`, nil,
	} {
		_, err := db.Exec(`UPDATE review_jobs SET token_usage = ? WHERE id = ?`, usage, jobs[i].ID)
		require.NoError(t, err)
		ids[i] = jobs[i].ID
	}
	return ids
}

func TestHumaListJobsSortedCursorPaging(t *testing.T) {
	srv, db, _ := newTestServer(t)
	ids := seedCostJobs(t, db)

	// Numeric sort keys round-trip through the JSON cursor; pages of two
	// split both tie groups.
	query := "sort=cost&direction=desc&limit=2"
	var got []int64
	page := fetchSortedJobsPage(t, srv, query)
	for {
		for _, job := range page.Jobs {
			got = append(got, job.ID)
		}
		if !page.HasMore {
			break
		}
		require.NotNil(t, page.NextCursor)
		page = fetchSortedJobsPage(t, srv, query+"&cursor="+url.QueryEscape(*page.NextCursor))
	}
	assert.Equal(t, []int64{ids[3], ids[2], ids[0], ids[4], ids[1]}, got)
}

func TestHumaListJobsRejectsInvalidSort(t *testing.T) {
	srv, db, _ := newTestServer(t)
	seedCostJobs(t, db)
	costCursor := fetchSortedJobsPage(t, srv, "sort=cost&limit=1").NextCursor
	require.NotNil(t, costCursor)

	for _, query := range []string{
		"sort=commit_subject",
		"direction=sideways",
		"sort=agent&before=3",
		"direction=asc&before=3",
		"sort=agent&cursor=" + url.QueryEscape(*costCursor),
		"sort=cost&direction=asc&cursor=" + url.QueryEscape(*costCursor),
	} {
		t.Run(query, func(t *testing.T) {
			rr := serveHuma(t, srv, http.MethodGet, fmt.Sprintf("/api/jobs?%s", query), nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
		})
	}
}
