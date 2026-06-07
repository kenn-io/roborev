package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListJobsParamsRepeatedRepo(t *testing.T) {
	assert := assert.New(t)

	values := neturl.Values{}
	values.Add("repo", "/path/to/backend-dev")
	values.Add("repo", "/path/to/backend-prod")

	params := listJobsParams(values)

	require.NotNil(t, params.Repo, "repeated repo values produce a slice filter")
	assert.Equal(
		[]string{"/path/to/backend-dev", "/path/to/backend-prod"},
		*params.Repo,
	)
}

func TestListJobsParamsNoRepo(t *testing.T) {
	params := listJobsParams(neturl.Values{})
	assert.Nil(t, params.Repo, "absent repo means no filter")
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

	cmd := m.fetchJobs()
	require.NotNil(t, cmd)
	_, ok := cmd().(jobsMsg)
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
