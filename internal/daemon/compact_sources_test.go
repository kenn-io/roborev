package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCompactSourcesEnqueue(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want int
	}{
		{"empty", http.StatusBadRequest},
		{"missing", http.StatusBadRequest},
		{"custom", http.StatusBadRequest},
		{"mixed custom", http.StatusBadRequest},
		{"custom voter", http.StatusBadRequest},
		{"custom skipped voter", http.StatusBadRequest},
		{"parent without run", http.StatusBadRequest},
		{"parent without members", http.StatusBadRequest},
		{"parent without voters", http.StatusBadRequest},
		{"built-in", http.StatusCreated},
		{"built-in parent", http.StatusCreated},
		{"custom observer", http.StatusCreated},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			s := NewServer(tc.DB, config.DefaultConfig(), "")
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			source, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, GitRef: "source", Agent: "test", ReviewType: "default",
			})
			require.NoError(t, err)
			sourceIDs := []int64{source.ID}
			switch scenario.name {
			case "empty":
				sourceIDs = nil
			case "missing":
				sourceIDs = []int64{source.ID, 99999}
			case "custom", "mixed custom":
				_, err = tc.DB.Exec("UPDATE review_jobs SET review_type = 'citations' WHERE id = ?", source.ID)
				require.NoError(t, err)
				if scenario.name == "mixed custom" {
					builtin, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "other", Agent: "test", ReviewType: "security"})
					require.NoError(t, err)
					sourceIDs = []int64{builtin.ID, source.ID}
				}
			case "parent without run":
				_, err = tc.DB.Exec("UPDATE review_jobs SET job_type = 'synthesis' WHERE id = ?", source.ID)
				require.NoError(t, err)
			case "built-in":
				// A direct built-in needs no member provenance.
			default:
				_, members, parent := enqueuePanelRun(t, tc, "drafts", []memberSpec{
					{name: "voter", agent: "test"},
					{name: "second", agent: "test", nonVoting: scenario.name == "custom observer"},
				})
				sourceIDs = []int64{parent.ID}
				if scenario.name == "custom voter" || scenario.name == "custom skipped voter" || scenario.name == "custom observer" {
					_, err = tc.DB.Exec("UPDATE review_jobs SET review_type = 'citations' WHERE id = ?", members[1].ID)
					require.NoError(t, err)
				}
				if scenario.name == "custom skipped voter" {
					_, err = tc.DB.Exec("UPDATE review_jobs SET status = 'skipped' WHERE id = ?", members[1].ID)
					require.NoError(t, err)
				}
				if scenario.name == "parent without members" {
					_, err = tc.DB.Exec("DELETE FROM review_jobs WHERE id IN (?, ?)", members[0].ID, members[1].ID)
					require.NoError(t, err)
				}
				if scenario.name == "parent without voters" {
					_, err = tc.DB.Exec("UPDATE review_jobs SET non_voting = 1 WHERE id IN (?, ?)", members[0].ID, members[1].ID)
					require.NoError(t, err)
				}
			}
			before, err := tc.DB.ListJobs("", "", 0, 0)
			require.NoError(t, err)
			request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", map[string]any{
				"repo_path": tc.TmpDir, "git_ref": "compact-drafts", "agent": "test",
				"custom_prompt": "Verify prior findings.", "job_type": "compact", "agentic": true,
				"source_job_ids": sourceIDs,
			})
			response := httptest.NewRecorder()
			s.httpServer.Handler.ServeHTTP(response, request)
			require.Equal(t, scenario.want, response.Code, response.Body.String())
			after, err := tc.DB.ListJobs("", "", 0, 0)
			require.NoError(t, err)
			if scenario.want == http.StatusBadRequest {
				assert.Len(t, after, len(before), "rejected provenance must not enqueue work")
				assert.Contains(t, response.Body.String(), "source")
			} else {
				assert.Len(t, after, len(before)+1)
			}
		})
	}
}
