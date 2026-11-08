package main

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestJobMetadataOutputSize(t *testing.T) {
	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	for range 50 {
		job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "test"})
		require.NoError(t, err)
		_, err = db.ClaimJob("test")
		require.NoError(t, err)
		require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", strings.Repeat("p", 8192), "No issues found."))
	}
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, tt := range []struct {
		name     string
		command  func() *cobra.Command
		args     []string
		count    int
		maxBytes int
	}{
		{"list", listCmd, []string{"--json", "--repo", repo.RootPath, "--all-branches"}, 50, 125000},
		{"status", statusCmd, []string{"--json"}, 10, 25000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			NewMockDaemon(t, MockRefineHooks{OnGetJobs: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				assert.Equal(t, "true", r.URL.Query().Get("omit_prompt"))
				server.Handler().ServeHTTP(w, r)
				return true
			}, OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				if r.URL.Path != "/api/health" {
					return false
				}
				assert.NoError(t, json.MarshalWrite(w, storage.HealthStatus{}))
				return true
			}})
			cmd := tt.command()
			cmd.SetArgs(tt.args)
			output := captureStdout(t, func() { require.NoError(t, cmd.Execute()) })
			var jobs []map[string]any
			if tt.name == "list" {
				require.NoError(t, json.Unmarshal([]byte(output), &jobs))
			} else {
				var result struct {
					Jobs []map[string]any `json:"jobs"`
				}
				require.NoError(t, json.Unmarshal([]byte(output), &result))
				jobs = result.Jobs
			}
			require.Len(t, jobs, tt.count)
			for _, job := range jobs {
				_, hasPrompt := job["prompt"]
				assert.False(t, hasPrompt, "job %v includes a prompt", job["id"])
			}
			assert.Less(t, len(output), tt.maxBytes)
			t.Logf("%d jobs: %d stdout bytes", len(jobs), len(output))
		})
	}
}
