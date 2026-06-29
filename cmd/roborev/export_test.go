package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportReviewsCmdFollowsCursors(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal(http.MethodGet, r.Method)
			assert.Equal("json", r.URL.Query().Get("format"))
			assert.Equal("metadata", r.URL.Query().Get("profile"))
			assert.Equal("2026-06-29", r.URL.Query().Get("since"))
			assert.Equal("2026-06-30", r.URL.Query().Get("until"))
			assert.Equal("true", r.URL.Query().Get("closed_only"))
			assert.Equal("github.com/acme/widgets", r.URL.Query().Get("repo"))
			assert.Equal("widgets", r.URL.Query().Get("project"))
			switch len(calls) {
			case 1:
				assert.Empty(r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("cursor-1"), []map[string]any{
					{"review_id": "r1", "content": nil},
				})
			case 2:
				assert.Equal("cursor-1", r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), false, nil, []map[string]any{
					{"review_id": "r2", "content": nil},
				})
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t,
		"reviews",
		"--profile", "metadata",
		"--since", "2026-06-29",
		"--until", "2026-06-30",
		"--closed-only",
		"--repo", "github.com/acme/widgets",
		"--project", "widgets",
	)

	require.Len(t, calls, 2)
	var got struct {
		Profile    string           `json:"profile"`
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal("metadata", got.Profile)
	assert.False(got.Truncated)
	assert.Nil(got.NextCursor)
	require.Len(t, got.Reviews, 2)
	assert.Equal("r1", got.Reviews[0]["review_id"])
	assert.Equal("r2", got.Reviews[1]["review_id"])
}

func TestExportReviewsCmdLimitStopsAtCursor(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			assert.Equal("1", r.URL.Query().Get("limit"))
			writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("next-page"), []map[string]any{
				{"review_id": "r1", "content": "raw"},
			})
			return true
		},
	})

	output := runExportCmd(t, "reviews", "--limit", "1")

	require.Len(t, calls, 1)
	var got struct {
		Profile    string           `json:"profile"`
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.Equal("content", got.Profile)
	assert.True(got.Truncated)
	require.NotNil(t, got.NextCursor)
	assert.Equal("next-page", *got.NextCursor)
	require.Len(t, got.Reviews, 1)
	assert.Equal("raw", got.Reviews[0]["content"])
}

func TestExportReviewsCmdExplicitLargeLimitFollowsUntilLimit(t *testing.T) {
	assert := assert.New(t)
	var calls []string
	NewMockDaemon(t, MockRefineHooks{
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			if r.URL.Path != "/api/export/reviews" {
				return false
			}
			calls = append(calls, r.URL.RawQuery)
			switch len(calls) {
			case 1:
				assert.Equal("5000", r.URL.Query().Get("limit"))
				assert.Empty(r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("first-page"), exportTestReviews(5000, 0))
			case 2:
				assert.Equal("1000", r.URL.Query().Get("limit"))
				assert.Equal("first-page", r.URL.Query().Get("cursor"))
				writeExportTestPage(t, w, r.URL.Query().Get("profile"), true, new("second-page"), exportTestReviews(1000, 5000))
			default:
				http.Error(w, "too many calls", http.StatusInternalServerError)
			}
			return true
		},
	})

	output := runExportCmd(t, "reviews", "--limit", "6000")

	require.Len(t, calls, 2)
	var got struct {
		Truncated  bool             `json:"truncated"`
		NextCursor *string          `json:"next_cursor"`
		Reviews    []map[string]any `json:"reviews"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	assert.True(got.Truncated)
	require.NotNil(t, got.NextCursor)
	assert.Equal("second-page", *got.NextCursor)
	require.Len(t, got.Reviews, 6000)
	assert.Equal("r-0000", got.Reviews[0]["review_id"])
	assert.Equal("r-5999", got.Reviews[5999]["review_id"])
}

func TestExportReviewsCmdRejectsInvalidFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "format", args: []string{"reviews", "--format", "yaml"}},
		{name: "profile", args: []string{"reviews", "--profile", "full"}},
		{name: "limit", args: []string{"reviews", "--limit", "0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exportCmd()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.Error(t, err)
			assert.False(t, cmd.SilenceUsage)
		})
	}
}

func TestRootExportReviewsCmdRejectsInvalidFlagsAsUsageError(t *testing.T) {
	cmd := &cobra.Command{
		Use: "roborev",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return nil
		},
	}
	cmd.AddCommand(exportCmd())
	cmd.SetArgs([]string{"export", "reviews", "--profile", "full"})

	executed, err := cmd.ExecuteC()
	require.Error(t, err)
	require.NotNil(t, executed)
	assert.Equal(t, "reviews", executed.Name())
	assert.False(t, executed.SilenceUsage)
}

func runExportCmd(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := exportCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	require.NoError(t, cmd.Execute())
	return out.String()
}

func writeExportTestPage(t *testing.T, w http.ResponseWriter, profile string, truncated bool, nextCursor *string, reviews []map[string]any) {
	t.Helper()
	if profile == "" {
		profile = "content"
	}
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"schema_version": 1,
		"tool":           "roborev",
		"tool_version":   "dev",
		"generated_at":   "2026-06-29T00:00:00Z",
		"profile":        profile,
		"window": map[string]any{
			"field": "completed_at",
			"since": nil,
			"until": nil,
		},
		"truncated":   truncated,
		"next_cursor": nextCursor,
		"reviews":     reviews,
	}))
}

func exportTestReviews(n, offset int) []map[string]any {
	reviews := make([]map[string]any, n)
	for i := range n {
		reviews[i] = map[string]any{
			"review_id": fmt.Sprintf("r-%04d", offset+i),
		}
	}
	return reviews
}
