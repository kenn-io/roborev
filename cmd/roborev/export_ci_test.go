package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCIMetricsTestPage(t *testing.T, w http.ResponseWriter, truncated bool, next *string, panels []map[string]any) {
	t.Helper()
	doc := map[string]any{
		"schema_version": 1,
		"tool":           "roborev",
		"tool_version":   "test",
		"generated_at":   "2026-07-12T00:00:00Z",
		"database_id":    testUUID("metrics-database"),
		"window":         map[string]any{"field": "posted_at", "since": nil, "until": nil},
		"truncated":      truncated,
		"next_cursor":    next,
		"panels":         panels,
	}
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(doc))
}

func TestExportCIMetricsCmdFollowsCursors(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-metrics" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal(http.MethodGet, r.Method)
			switch len(calls) {
			case 1:
				assert.Equal("2026-07-01", r.URL.Query().Get("since"))
				assert.Empty(r.URL.Query().Get("cursor"))
				writeCIMetricsTestPage(t, w, true, new("cursor-1"), []map[string]any{
					{"github_repo": "o/r", "pr_number": 1, "head_sha": "a", "outcome": "review_posted", "jobs": []any{}},
				})
			case 2:
				assert.Empty(r.URL.Query().Get("since"))
				assert.Equal("cursor-1", r.URL.Query().Get("cursor"))
				writeCIMetricsTestPage(t, w, false, new("cursor-2"), []map[string]any{
					{"github_repo": "o/r", "pr_number": 2, "head_sha": "b", "outcome": "giveup_posted", "jobs": []any{}},
				})
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t, "ci-metrics", "--since", "2026-07-01")

	require.Len(t, calls, 2)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &doc))
	panels := doc["panels"].([]any)
	assert.Len(panels, 2)
	assert.Equal(false, doc["truncated"])
}

func TestExportCIMetricsCmdLegacyFlagSetsQueryParam(t *testing.T) {
	assert := assert.New(t)
	var sawLegacy string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-metrics" {
				return false
			}
			sawLegacy = r.URL.Query().Get("legacy")
			writeCIMetricsTestPage(t, w, false, nil, []map[string]any{
				{"github_repo": "o/r", "pr_number": 1, "head_sha": "a", "outcome": "legacy_review", "jobs": []any{}},
			})
			return true
		},
	})

	runExportCmd(t, "ci-metrics", "--legacy")

	assert.Equal("true", sawLegacy)
}

func TestExportCIMetricsCmdWithoutLegacyFlagOmitsQueryParam(t *testing.T) {
	assert := assert.New(t)
	var sawLegacy string
	sawParam := false
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/ci-metrics" {
				return false
			}
			sawParam = r.URL.Query().Has("legacy")
			sawLegacy = r.URL.Query().Get("legacy")
			writeCIMetricsTestPage(t, w, false, nil, []map[string]any{
				{"github_repo": "o/r", "pr_number": 1, "head_sha": "a", "outcome": "review_posted", "jobs": []any{}},
			})
			return true
		},
	})

	runExportCmd(t, "ci-metrics")

	assert.False(sawParam, "legacy query param must be omitted when --legacy is not set")
	assert.Empty(sawLegacy)
}
