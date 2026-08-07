package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func writeCICostTestPage(t *testing.T, w http.ResponseWriter, truncated bool, next *string, jobs []map[string]any) {
	t.Helper()
	doc := map[string]any{
		"schema_version": 1,
		"tool":           "roborev",
		"tool_version":   "test",
		"generated_at":   "2026-08-06T12:00:00Z",
		"database_id":    "database-1",
		"legacy":         false,
		"window":         map[string]any{"field": "finished_at", "since": nil, "until": nil},
		"truncated":      truncated,
		"next_cursor":    next,
		"jobs":           jobs,
	}
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(doc))
}

func TestExportCICostCmdFollowsCursors(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-costs" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal(http.MethodGet, r.Method)
			assert.Equal("json", r.URL.Query().Get("format"))
			switch len(calls) {
			case 1:
				assert.Equal("2026-08-01", r.URL.Query().Get("since"))
				assert.Equal("2026-08-07", r.URL.Query().Get("until"))
				assert.Empty(r.URL.Query().Get("cursor"))
				writeCICostTestPage(t, w, true, new("cursor-1"), []map[string]any{
					{"job_uuid": "job-1", "finished_at": "2026-08-01T01:00:00Z", "agent": "agent-a", "role": "member", "status": "done", "cost_usd": 0.25},
				})
			case 2:
				assert.Empty(r.URL.Query().Get("since"))
				assert.Empty(r.URL.Query().Get("until"))
				assert.Equal("cursor-1", r.URL.Query().Get("cursor"))
				writeCICostTestPage(t, w, false, new("cursor-2"), []map[string]any{
					{"job_uuid": "job-2", "finished_at": "2026-08-01T02:00:00Z", "agent": "agent-b", "role": "synthesis", "status": "failed", "cost_usd": nil},
				})
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t, "ci-costs", "--since", "2026-08-01", "--until", "2026-08-07")
	require.Len(t, calls, 2)
	var doc struct {
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Jobs       []map[string]any `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &doc))
	assert.False(doc.Truncated)
	require.NotNil(t, doc.NextCursor)
	assert.Equal("cursor-2", *doc.NextCursor)
	require.Len(t, doc.Jobs, 2)
	assert.Equal("job-1", doc.Jobs[0]["job_uuid"])
	assert.Equal("job-2", doc.Jobs[1]["job_uuid"])
}

func TestExportCICostCmdAutoPaginationPreservesSince(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	seed := func(gitRef, finishedAt string) *storage.ReviewJob {
		job, enqueueErr := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: gitRef, Agent: "test-agent",
			Source: storage.JobSourceCI, PanelRole: storage.PanelRoleMember,
			PanelRunUUID: "run-" + gitRef,
		})
		require.NoError(t, enqueueErr)
		_, updateErr := db.Exec(`UPDATE review_jobs
			SET status = 'done', started_at = ?, finished_at = ?,
			    updated_at = ?, agent_invoked = 1, token_usage = '{}'
			WHERE id = ?`, finishedAt, finishedAt, finishedAt, job.ID)
		require.NoError(t, updateErr)
		return job
	}
	oldJob := seed("old", "2026-07-01 01:00:00")
	firstJob := seed("first", "2026-08-01 01:00:00")
	secondJob := seed("second", "2026-08-01 02:00:00")
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sinceText := since.Format(time.RFC3339)

	callCount := 0
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-costs" {
				return false
			}
			callCount++
			opts := storage.ExportCICostOptions{
				Cursor: r.URL.Query().Get("cursor"), Limit: 1,
			}
			var windowSince *string
			if r.URL.Query().Get("since") != "" {
				require.Equal(t, "2026-08-01", r.URL.Query().Get("since"))
				opts.Since = since
				windowSince = &sinceText
			}
			page, exportErr := db.ExportCICosts(opts)
			require.NoError(t, exportErr)
			if callCount == 1 {
				_, updateErr := db.Exec(`UPDATE review_jobs
					SET token_usage = '{"has_cost":true,"cost_usd":0.75}'
					WHERE id = ?`, oldJob.ID)
				require.NoError(t, updateErr)
			}
			databaseID, idErr := db.GetDatabaseID()
			require.NoError(t, idErr)
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(daemon.ExportCICostDocument{
				SchemaVersion: 1,
				Tool:          "roborev",
				ToolVersion:   "test",
				GeneratedAt:   "2026-08-07T12:00:00Z",
				DatabaseID:    databaseID,
				Window: daemon.ExportReviewsWindow{
					Field: "finished_at", Since: windowSince,
				},
				Truncated:  page.Truncated,
				NextCursor: page.NextCursor,
				Jobs:       page.Jobs,
			}))
			return true
		},
	})

	output := runExportCmd(t, "ci-costs", "--since", "2026-08-01")
	var doc daemon.ExportCICostDocument
	require.NoError(t, json.Unmarshal([]byte(output), &doc))
	assert.Equal(t, 2, callCount)
	require.NotNil(t, doc.Window.Since)
	assert.Equal(t, sinceText, *doc.Window.Since)
	require.Len(t, doc.Jobs, 2)
	assert.Equal(t, firstJob.UUID, doc.Jobs[0].JobUUID)
	assert.Equal(t, secondJob.UUID, doc.Jobs[1].JobUUID)
}

func TestExportCICostCmdPropagatesLimitAndLegacy(t *testing.T) {
	assert := assert.New(t)
	callCount := 0
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-costs" {
				return false
			}
			callCount++
			assert.Equal("1000", r.URL.Query().Get("limit"))
			assert.Equal("true", r.URL.Query().Get("legacy"))
			jobs := make([]map[string]any, 1000)
			for i := range jobs {
				jobs[i] = map[string]any{
					"job_uuid": "job", "finished_at": "2026-08-01T01:00:00Z",
					"agent": "agent-a", "role": "review", "status": "done", "cost_usd": 0,
				}
			}
			writeCICostTestPage(t, w, true, new("next-page"), jobs)
			return true
		},
	})

	output := runExportCmd(t, "ci-costs", "--legacy", "--limit", "1000")
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &doc))
	assert.Equal(true, doc["truncated"])
	assert.Equal("next-page", doc["next_cursor"])
	assert.Equal(1, callCount)
}

func TestExportCICostCmdValidatesOptions(t *testing.T) {
	for _, args := range [][]string{
		{"ci-costs", "--format", "csv"},
		{"ci-costs", "--limit", "0"},
		{"ci-costs", "--limit", "-1"},
		{"ci-costs", "--cursor", "cursor", "--since", "2026-08-01"},
		{"ci-costs", "--cursor", "cursor", "--until", "2026-08-02"},
	} {
		cmd := exportCmd()
		cmd.SetArgs(args)
		err := cmd.Execute()
		require.Error(t, err, args)
	}
}

func TestExportCICostCmdRejectsInvalidPagination(t *testing.T) {
	for _, tc := range []struct {
		name string
		next *string
		jobs []map[string]any
	}{
		{name: "missing cursor", jobs: []map[string]any{{"job_uuid": "job-1"}}},
		{name: "empty page", next: new("cursor-1"), jobs: []map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
					if r.URL.Path != "/api/export/ci-costs" {
						return false
					}
					writeCICostTestPage(t, w, true, tc.next, tc.jobs)
					return true
				},
			})
			cmd := exportCmd()
			cmd.SetArgs([]string{"ci-costs"})
			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "truncated export page")
		})
	}
}

func TestExportCICostCmdUsesCursorResetExitCode(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-costs" {
				return false
			}
			http.Error(w, "export cursor database reset", http.StatusConflict)
			return true
		},
	})
	cmd := exportCmd()
	cmd.SetArgs([]string{"ci-costs", "--cursor", "old-cursor"})
	err := cmd.Execute()
	require.Error(t, err)
	var exitErr *exitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, exportReviewsCursorResetExitCode, exitErr.code)
}
