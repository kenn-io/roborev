package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func TestParseSinceDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		wantErr bool
		checkFn func(t *testing.T, got time.Time)
	}{
		{
			input: "7d",
			checkFn: func(t *testing.T, got time.Time) {
				t.Helper()
				expected := time.Now().AddDate(0, 0, -7)
				assertDurationApprox(t, expected, got, "7d")
			},
		},
		{
			input: "30d",
			checkFn: func(t *testing.T, got time.Time) {
				t.Helper()
				expected := time.Now().AddDate(0, 0, -30)
				assertDurationApprox(t, expected, got, "30d")
			},
		},
		{
			input: "2w",
			checkFn: func(t *testing.T, got time.Time) {
				t.Helper()
				expected := time.Now().AddDate(0, 0, -14)
				assertDurationApprox(t, expected, got, "2w")
			},
		},
		{
			input: "720h",
			checkFn: func(t *testing.T, got time.Time) {
				t.Helper()
				expected := time.Now().Add(-720 * time.Hour)
				assertDurationApprox(t, expected, got, "720h")
			},
		},
		{
			input: "",
			checkFn: func(t *testing.T, got time.Time) {
				t.Helper()
				expected := time.Now().AddDate(0, 0, -30)
				assertDurationApprox(t, expected, got, "empty")
			},
		},
		{input: "invalid", wantErr: true},
		{input: "0d", wantErr: true},
		{input: "-5d", wantErr: true},
		{input: "-5h", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseSinceDuration(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			if tt.checkFn != nil {
				tt.checkFn(t, got)
			}
		})
	}
}

func TestRunInsights_EnqueuesInsightsJob(t *testing.T) {
	repo := NewGitTestRepo(t)
	repo.CommitFile("README.md", "hello\n", "init")
	repo.Run("checkout", "-b", "feature")

	var enqueued daemon.EnqueueRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		err := json.NewDecoder(r.Body).Decode(&enqueued)
		assert.NoError(t, err)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, storage.ReviewJob{
			ID:     99,
			Agent:  "codex",
			Status: storage.JobStatusQueued,
		})
	})

	daemonFromHandler(t, mux)

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := runInsights(t.Context(), cmd, insightsOptions{
		repoPath: repo.Dir,
		branch:   "main",
		since:    "7d",
		wait:     false,
	})
	require.NoError(t, err)

	require.Equal(t, storage.JobTypeInsights, enqueued.JobType)
	require.Equal(t, "insights", enqueued.GitRef)
	assert.Equal(t, "main", enqueued.Branch)
	assert.Empty(t, enqueued.CustomPrompt)

	since, err := time.Parse(time.RFC3339, enqueued.Since)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().AddDate(0, 0, -7), since, 2*time.Second)
}

func assertDurationApprox(
	t *testing.T, expected, got time.Time, label string,
) {
	t.Helper()
	diff := got.Sub(expected)
	assert.LessOrEqual(t, diff, time.Second, "%s upper bound", label)
	assert.GreaterOrEqual(t, diff, -time.Second, "%s lower bound", label)
}
