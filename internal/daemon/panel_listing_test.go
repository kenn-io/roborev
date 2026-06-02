package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// listJobsViaHTTP issues GET /api/jobs<query> and returns the decoded rows.
// query must start with "?" or be empty.
func listJobsViaHTTP(t *testing.T, server *Server, query string) []storage.ReviewJob {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs"+query, nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	return body.Jobs
}

// enqueueTrioPanel sets up a repo with the trio panel and enqueues one run,
// returning the run uuid, synthesis (parent) job id, and the frozen git ref.
func enqueueTrioPanel(t *testing.T, server *Server) (runUUID string, synthID int64, frozenRef string) {
	t.Helper()
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})
	return resp.PanelRunUUID, resp.ID, resp.GitRef
}

// TestListJobsExcludesMembersByDefault verifies the default listing is
// parent-only: the synthesis row is present, the three member rows are not.
func TestListJobsExcludesMembersByDefault(t *testing.T) {
	assert := assert.New(t)
	server, _, _ := newTestServer(t)

	runUUID, synthID, _ := enqueueTrioPanel(t, server)

	jobs := listJobsViaHTTP(t, server, "")

	var sawSynth bool
	for _, j := range jobs {
		assert.NotEqual(storage.PanelRoleMember, j.PanelRole,
			"member rows must be excluded from the default listing")
		if j.ID == synthID {
			sawSynth = true
		}
	}
	assert.True(sawSynth, "synthesis parent must appear in the default listing")
	assert.NotEmpty(runUUID)
}

// TestListJobsPanelRunReturnsMembers verifies panel_run=<uuid> expands a run:
// all three members plus the synthesis row are returned.
func TestListJobsPanelRunReturnsMembers(t *testing.T) {
	assert := assert.New(t)
	server, _, _ := newTestServer(t)

	runUUID, synthID, _ := enqueueTrioPanel(t, server)

	jobs := listJobsViaHTTP(t, server, "?panel_run="+runUUID)

	var members int
	var sawSynth bool
	for _, j := range jobs {
		assert.Equal(runUUID, j.PanelRunUUID)
		switch j.PanelRole {
		case storage.PanelRoleMember:
			members++
		case storage.PanelRoleSynthesis:
			sawSynth = true
			assert.Equal(synthID, j.ID)
		}
	}
	assert.Equal(3, members, "panel_run must return all member rows")
	assert.True(sawSynth, "panel_run must include the synthesis row")
}

// TestFindJobForCommitResolvesToSynthesis verifies the wait/show SHA path:
// a git_ref filter (members share the frozen ref) resolves to the synthesis
// parent, never a member.
func TestFindJobForCommitResolvesToSynthesis(t *testing.T) {
	assert := assert.New(t)
	server, _, _ := newTestServer(t)

	_, synthID, frozenRef := enqueueTrioPanel(t, server)

	// No limit: assert the entire matched set for the frozen ref is
	// parent-only, so this guards member-exclusion directly rather than
	// passing by id-ordering coincidence (the synthesis happens to have the
	// highest id, so a limit=1 query would return it regardless).
	jobs := listJobsViaHTTP(t, server, "?git_ref="+frozenRef)

	require.Len(t, jobs, 1, "default listing for the frozen ref is parent-only")
	for _, j := range jobs {
		assert.NotEqual(storage.PanelRoleMember, j.PanelRole,
			"no member row may match the SHA-resolution path")
	}
	assert.Equal(synthID, jobs[0].ID, "SHA resolution must return the synthesis parent")
}

// TestListJobsAttachesPanelSummary verifies a synthesis parent in the listing
// carries an additive panel_summary with the run's member breakdown, while a
// non-panel job carries none.
func TestListJobsAttachesPanelSummary(t *testing.T) {
	assert := assert.New(t)
	server, _, _ := newTestServer(t)

	_, synthID, _ := enqueueTrioPanel(t, server)

	jobs := listJobsViaHTTP(t, server, "")

	var checked bool
	for i := range jobs {
		j := jobs[i]
		if j.ID == synthID {
			checked = true
			require.NotNil(t, j.PanelSummary, "synthesis parent must carry panel_summary")
			assert.Equal(3, j.PanelSummary.MembersTotal)
			assert.Equal(0, j.PanelSummary.MembersTerminal,
				"freshly enqueued members are not yet terminal")
		} else if j.PanelRole == "" {
			assert.Nil(j.PanelSummary, "non-panel jobs carry no panel_summary")
		}
	}
	assert.True(checked, "expected to see the synthesis parent in the listing")
}

// TestListJobsStatsExcludeMembers verifies the listing's aggregate stats count
// a panel run as its single synthesis parent, not its N members — keeping the
// queue header consistent with the parent-only rows.
func TestListJobsStatsExcludeMembers(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	runUUID, _, _ := enqueueTrioPanel(t, server)

	// Mark the whole run done so it counts toward stats (3 members + 1 synth).
	_, err := db.Exec(`UPDATE review_jobs SET status = 'done' WHERE panel_run_uuid = ?`, runUUID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Stats *storage.JobStats `json:"stats"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
	require.NotNil(t, body.Stats)
	assert.Equal(1, body.Stats.Done,
		"stats count the synthesis parent only, not the 3 members")
}
