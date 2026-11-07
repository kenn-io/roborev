package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

func TestFetchJobsFirstReview(t *testing.T) {
	t.Parallel()
	for _, closed := range []int{0, 1} {
		t.Run(strconv.Itoa(closed), func(t *testing.T) {
			_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/jobs", r.URL.Path)
				assert.Equal(t, []string{"/test/repo"}, r.URL.Query()["repo"])
				assert.Equal(t, "main", r.URL.Query().Get("branch"))
				assert.Equal(t, "false", r.URL.Query().Get("closed"))
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"jobs": []storage.ReviewJob{}, "has_more": false, "stats": storage.JobStats{Closed: closed},
				}))
			})
			m.width, m.height = 120, 24
			m.cwdRepoRoot, m.cwdBranch = "/test/repo", "main"
			m.activeRepoFilter, m.activeBranchFilter = []string{m.cwdRepoRoot}, m.cwdBranch
			m.autoRepoFilter, m.hideClosed = true, true
			m, _ = updateModel(t, m, fetchJobsMessage(t, m))
			output := m.renderQueueView() + strings.Join(m.renderQueuePaneBody(120, 12), "\n")
			if closed == 0 {
				assert.Contains(t, output, "No reviews yet")
			} else {
				assert.Contains(t, output, "No jobs matching filters")
				assert.NotContains(t, output, "No reviews yet")
			}
		})
	}
}

func TestListJobsParamsRepeatedRepo(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	query := listJobsQuery(neturl.Values{})
	require.NotNil(t, query)
	assert.Nil(t, query.Repo, "absent repo means no filter")
}

// A display name spanning multiple repos must scope the jobs query
// server-side (one ?repo= per path) and keep pagination, rather than
// falling back to limit=0 and loading every job — the regression that
// crashed the daemon on large databases.
func TestFetchJobsMultiRepoUsesRepeatedRepoAndPaginates(t *testing.T) {
	t.Parallel()
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

func TestFetchJobsNoneBranchFilterPaginates(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	var queries []neturl.Values
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jobs" {
			queries = append(queries, r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jobs": []any{}, "has_more": true})
	})
	m.activeBranchFilter = branchNone
	m.hideClosed = true
	m.heightDetected = true
	_, ok := fetchJobsMessage(t, m).(jobsMsg)
	require.True(t, ok)
	m.jobs = []storage.ReviewJob{{ID: 1}}
	more, ok := m.fetchMoreJobs()().(jobsMsg)
	require.True(t, ok)
	assert.True(more.append)
	require.Len(t, queries, 2)
	assert.Equal("1", queries[1].Get("offset"))
	assert.Equal(strconv.Itoa(m.queueVisibleRows()+queuePrefetchBuffer), queries[0].Get("limit"))
	for _, query := range queries {
		assert.Equal("true", query.Get("branch_empty"))
		assert.Equal("false", query.Get("closed"))
		assert.NotContains(query, "branch")
	}
}

// Queue/task/panel listings never render completed jobs' prompts, so every
// list fetch must request metadata-only rows: full prompts are ~91% of the
// payload and repeated polls were the daemon's main allocation driver.
// Detail views (single job, /api/review) still fetch the full record.
func TestListFetchesRequestMetadataOnlyRows(t *testing.T) {
	t.Parallel()
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

//nolint:paralleltest // sets the package-level version.Distribution
func TestCheckForUpdateOnlyForSelfUpdatingBuilds(t *testing.T) {
	original := version.Distribution
	t.Cleanup(func() { version.Distribution = original })

	version.Distribution = "homebrew"
	assert.Nil(t, model{}.checkForUpdate(), "package-managed builds skip the update check")

	// Official direct downloads ("kenn") update themselves, so they keep
	// the update check and notification.
	for _, distribution := range []string{"", "kenn"} {
		version.Distribution = distribution
		assert.NotNil(t, model{}.checkForUpdate(), "distribution %q", distribution)
	}
}
