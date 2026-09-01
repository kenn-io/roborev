package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestListJobsParamsRepeatedRepo(t *testing.T) {
	assert := assert.New(t)

	values := neturl.Values{}
	values.Add("repo", "/path/to/backend-dev")
	values.Add("repo", "/path/to/backend-prod")

	query := listJobsQuery(values)

	require.NotNil(t, query)
	require.NotNil(t, query.Repo, "repeated repo values produce a slice filter")
	assert.Equal(
		[]string{"/path/to/backend-dev", "/path/to/backend-prod"},
		query.Repo,
	)
}

func TestListJobsParamsNoRepo(t *testing.T) {
	query := listJobsQuery(neturl.Values{})
	require.NotNil(t, query)
	assert.Nil(t, query.Repo, "absent repo means no filter")
}

// A display name spanning multiple repos must scope the jobs query
// server-side (one ?repo= per path) and keep pagination, rather than
// falling back to limit=0 and loading every job — the regression that
// crashed the daemon on large databases.
func TestFetchJobsMultiRepoUsesRepeatedRepoAndPaginates(t *testing.T) {
	assert := assert.New(t)

	var jobsQuery neturl.Values
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/jobs" {
				jobsQuery = r.URL.Query()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"jobs": []any{}, "has_more": false,
			})
		},
	))
	defer ts.Close()

	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
	m.activeRepoFilter = []string{
		"/path/to/backend-dev", "/path/to/backend-prod",
	}

	_, ok := fetchJobsMessage(t, m).(jobsMsg)
	require.True(t, ok)

	require.NotNil(t, jobsQuery, "jobs endpoint should have been called")
	assert.ElementsMatch(
		[]string{"/path/to/backend-dev", "/path/to/backend-prod"},
		jobsQuery["repo"],
		"both repo paths sent as repeated params",
	)
	assert.NotEqual("0", jobsQuery.Get("limit"),
		"multi-repo paginates instead of loading every job")
}

// Queue/task/panel listings never render completed jobs' prompts, so every
// list fetch must request metadata-only rows: full prompts are ~91% of the
// payload and repeated polls were the daemon's main allocation driver.
// Detail views (single job, /api/review) still fetch the full record.
func TestListFetchesRequestMetadataOnlyRows(t *testing.T) {
	assert := assert.New(t)

	var mu sync.Mutex
	queries := make(map[string]neturl.Values)
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/jobs" {
				mu.Lock()
				key := "list"
				switch {
				case r.URL.Query().Get("panel_run") != "":
					key = "panel"
				case r.URL.Query().Get("job_type") == "fix":
					key = "fix"
				case r.URL.Query().Get("offset") != "":
					key = "more"
				}
				queries[key] = r.URL.Query()
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"jobs": []any{}, "has_more": false,
			})
		},
	))
	defer ts.Close()

	m := newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())

	_, ok := fetchJobsMessage(t, m).(jobsMsg)
	require.True(t, ok)
	m.jobs = []storage.ReviewJob{{ID: 1}}
	m.fetchMoreJobs()()
	m.fetchFixJobs()()
	m.fetchPanelMembers(testUUID("panel-run-uuid"))()

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{"list", "more", "fix", "panel"} {
		require.Contains(t, queries, key, "%s fetch should have hit /api/jobs", key)
		assert.Equal("true", queries[key].Get("omit_prompt"),
			"%s fetch must request metadata-only rows", key)
	}
}
