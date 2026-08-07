package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal("2026-08-07", r.URL.Query().Get("until"))
			switch len(calls) {
			case 1:
				assert.Equal("2026-08-01", r.URL.Query().Get("since"))
				assert.Empty(r.URL.Query().Get("cursor"))
				writeCICostTestPage(t, w, true, new("cursor-1"), []map[string]any{
					{"job_uuid": "job-1", "finished_at": "2026-08-01T01:00:00Z", "agent": "agent-a", "role": "member", "status": "done", "cost_usd": 0.25},
				})
			case 2:
				assert.Empty(r.URL.Query().Get("since"))
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
