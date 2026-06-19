package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

// createRepoWithConfig creates a temp directory with a .roborev.toml file.
// If configContent is empty, no config file is written.
func createRepoWithConfig(t *testing.T, configContent string) string {
	t.Helper()
	repoPath := t.TempDir()
	if configContent != "" {
		if err := os.WriteFile(filepath.Join(repoPath, ".roborev.toml"), []byte(configContent), 0o644); err != nil {
			require.NoError(t, err)
		}
	}
	return repoPath
}

type mockServerConfig struct {
	jobs        []storage.ReviewJob
	review      *storage.Review
	status      int // Default 200
	receivedRef *string
}

// newRunTestServer creates a unified test server for run command tests.
func newRunTestServer(t *testing.T, cfg mockServerConfig) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cfg.status != 0 && cfg.status != 200 {
			w.WriteHeader(cfg.status)
			if cfg.status == http.StatusInternalServerError {
				w.Write([]byte("server error"))
			}
			return
		}

		switch r.URL.Path {
		case "/api/ping":
			writeJSON(w, daemon.PingInfo{OK: true, Service: "roborev", Version: version.Version})
		case "/api/status":
			// Required for ensureDaemon()
			writeJSON(w, map[string]string{"version": version.Version})
		case "/api/jobs":
			writeJSON(w, map[string][]storage.ReviewJob{"jobs": cfg.jobs})
		case "/api/review":
			if cfg.review != nil {
				writeJSON(w, *cfg.review)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case "/api/enqueue":
			if r.Method == "POST" {
				var req daemon.EnqueueRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if cfg.receivedRef != nil {
					*cfg.receivedRef = req.GitRef
				}
				w.WriteHeader(http.StatusCreated)
				writeJSON(w, storage.ReviewJob{
					ID:     1,
					Agent:  "test",
					GitRef: req.GitRef,
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// stubReview creates a storage.Review with common defaults.
var nextStubReviewID atomic.Int64

func stubReview(jobID int64, agent, output string) storage.Review {
	return storage.Review{
		ID:     nextStubReviewID.Add(1),
		JobID:  jobID,
		Agent:  agent,
		Output: output,
	}
}

func TestBuildPromptWithContext(t *testing.T) {
	t.Run("includes repo name and path", func(t *testing.T) {
		repoPath := "/path/to/my-project"
		userPrompt := "Explain this code"

		result := buildPromptWithContext(repoPath, userPrompt)

		expectedStrings := []string{"my-project", repoPath, "## Context", "## Request", userPrompt}
		for _, s := range expectedStrings {
			assert.Contains(t, result, s)
		}
	})

	t.Run("includes project guidelines when present", func(t *testing.T) {
		repoPath := createRepoWithConfig(t, `review_guidelines = "Always use tabs for indentation"`)

		result := buildPromptWithContext(repoPath, "test prompt")

		assert.Contains(t, result, "## Project Guidelines")
		assert.Contains(t, result, "Always use tabs for indentation")
	})

	t.Run("appends global and project guidelines", func(t *testing.T) {
		dataDir := t.TempDir()
		t.Setenv("ROBOREV_DATA_DIR", dataDir)
		require.NoError(t, os.MkdirAll(dataDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"),
			[]byte(`review_guidelines = "Global rule"`+"\n"), 0o644))
		repoPath := createRepoWithConfig(t, `review_guidelines = "Project rule"`)

		result := buildPromptWithContext(repoPath, "test prompt")

		assert.Contains(t, result, "Global rule")
		assert.Contains(t, result, "Project rule")
		assert.Less(t, strings.Index(result, "Global rule"), strings.Index(result, "Project rule"))
	})

	t.Run("omits guidelines section when not configured", func(t *testing.T) {
		repoPath := createRepoWithConfig(t, "")

		result := buildPromptWithContext(repoPath, "test prompt")

		assert.NotContains(t, result, "## Project Guidelines")
	})

	t.Run("omits guidelines when config has no guidelines", func(t *testing.T) {
		repoPath := createRepoWithConfig(t, `agent = "claude-code"`)

		result := buildPromptWithContext(repoPath, "test prompt")

		assert.NotContains(t, result, "## Project Guidelines")
	})

	t.Run("preserves user prompt exactly", func(t *testing.T) {
		repoPath := "/tmp/test"
		userPrompt := "Find all TODO comments\nand list them"

		result := buildPromptWithContext(repoPath, userPrompt)

		assert.Contains(t, result, userPrompt)
	})

	t.Run("correct section order", func(t *testing.T) {
		repoPath := createRepoWithConfig(t, `review_guidelines = "Test guideline"`)

		result := buildPromptWithContext(repoPath, "test prompt")

		contextPos := strings.Index(result, "## Context")
		guidelinesPos := strings.Index(result, "## Project Guidelines")
		requestPos := strings.Index(result, "## Request")

		require.NotEqual(t, -1, contextPos, "Missing expected sections")
		require.NotEqual(t, -1, guidelinesPos, "Missing expected sections")
		require.NotEqual(t, -1, requestPos, "Missing expected sections")

		assert.LessOrEqual(t, contextPos, guidelinesPos)
		assert.LessOrEqual(t, guidelinesPos, requestPos)
	})
}

func TestShowPromptResult(t *testing.T) {
	t.Run("displays result without verdict exit code", func(t *testing.T) {
		review := stubReview(123, "test-agent", "Paris")
		server := newRunTestServer(t, mockServerConfig{review: &review})

		cmd, out := newTestCmd(t)
		err := showPromptResult(cmd, server.URL, 123, false, "")

		require.NoError(t, err)

		output := out.String()
		assert.Contains(t, output, "Result (by test-agent)")
		assert.Contains(t, output, "Paris")
	})

	t.Run("returns nil for output that would be FAIL verdict in review", func(t *testing.T) {
		review := stubReview(123, "test-agent", "Found several issues:\n1. Bug in line 5\n2. Missing error handling")
		server := newRunTestServer(t, mockServerConfig{review: &review})

		cmd, _ := newTestCmd(t)
		err := showPromptResult(cmd, server.URL, 123, false, "")

		require.NoError(t, err)
	})

	t.Run("quiet mode suppresses output", func(t *testing.T) {
		review := stubReview(123, "test-agent", "Some output")
		server := newRunTestServer(t, mockServerConfig{review: &review})

		cmd, out := newTestCmd(t)
		err := showPromptResult(cmd, server.URL, 123, true, "") // quiet=true

		require.NoError(t, err)

		assert.LessOrEqual(t, out.Len(), 0)
	})

	t.Run("handles not found error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)

		cmd, _ := newTestCmd(t)
		err := showPromptResult(cmd, server.URL, 999, false, "")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no result found")
	})

	t.Run("handles server error", func(t *testing.T) {
		server := newRunTestServer(t, mockServerConfig{status: http.StatusInternalServerError})
		cmd, _ := newTestCmd(t)
		err := showPromptResult(cmd, server.URL, 123, false, "")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "server error")
	})
}

func TestRunLabelFlag(t *testing.T) {
	tests := []struct {
		name        string
		label       string
		expectedRef string
	}{
		{
			name:        "no label defaults to run",
			label:       "",
			expectedRef: "run",
		},
		{
			name:        "custom label is used",
			label:       "my-task",
			expectedRef: "my-task",
		},
		{
			name:        "analyze label",
			label:       "analyze",
			expectedRef: "analyze",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedRef string
			server := newRunTestServer(t, mockServerConfig{receivedRef: &receivedRef})
			patchServerAddr(t, server.URL)

			cmd := runCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			args := []string{"test prompt"}
			if tt.label != "" {
				args = append(args, "--label", tt.label)
			}
			cmd.SetArgs(args)

			err := cmd.Execute()
			require.NoError(t, err)

			assert.Equal(t, tt.expectedRef, receivedRef)
		})
	}
}

func newPollingTestServer(t *testing.T, statuses []storage.JobStatus) (*httptest.Server, *int) {
	t.Helper()
	pollCount := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			status := statuses[len(statuses)-1] // Default to last
			if pollCount < len(statuses) {
				status = statuses[pollCount]
			}
			pollCount++

			writeJSON(w, map[string][]storage.ReviewJob{"jobs": {{ID: 123, Status: status}}})
		case "/api/review":
			writeJSON(w, stubReview(123, "test-agent", "Final result"))
		}
	}))
	t.Cleanup(s.Close)
	return s, &pollCount
}

func TestWaitForPromptJob(t *testing.T) {
	review := stubReview(123, "test-agent", "Result")

	tests := []struct {
		name        string
		jobs        []storage.ReviewJob
		review      *storage.Review
		quiet       bool
		expectError string
		expectOut   []string
	}{
		{
			name:      "success",
			jobs:      []storage.ReviewJob{{ID: 123, Status: storage.JobStatusDone}},
			review:    &review,
			expectOut: []string{"done!", "Result"},
		},
		{
			name:        "failed job",
			jobs:        []storage.ReviewJob{{ID: 123, Status: storage.JobStatusFailed, Error: "oops"}},
			expectError: "oops",
			expectOut:   []string{"failed!"},
		},
		{
			name:        "canceled job",
			jobs:        []storage.ReviewJob{{ID: 123, Status: storage.JobStatusCanceled}},
			expectError: "canceled",
			expectOut:   []string{"canceled!"},
		},
		{
			name:        "job not found",
			jobs:        []storage.ReviewJob{},
			expectError: "not found",
		},
		{
			name:      "quiet mode success",
			jobs:      []storage.ReviewJob{{ID: 123, Status: storage.JobStatusDone}},
			review:    &review,
			quiet:     true,
			expectOut: []string{}, // Should be empty
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newRunTestServer(t, mockServerConfig{jobs: tt.jobs, review: tt.review})
			cmd, out := newTestCmd(t)

			// Use small poll interval for tests
			err := waitForPromptJob(cmd, mustParseEndpoint(t, server.URL), 123, tt.quiet, 1*time.Millisecond)

			if tt.expectError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectError, "Expected error containing %q, got %v", tt.expectError, err)
			} else {
				require.NoError(t, err)
			}

			output := out.String()
			if tt.quiet {
				assert.Empty(t, output)
			} else {
				for _, s := range tt.expectOut {
					assert.Contains(t, output, s)
				}
			}
		})
	}

	t.Run("polls while job is running", func(t *testing.T) {
		server, pollCount := newPollingTestServer(t, []storage.JobStatus{
			storage.JobStatusRunning,
			storage.JobStatusRunning,
			storage.JobStatusDone,
		})

		cmd, _ := newTestCmd(t)
		err := waitForPromptJob(cmd, mustParseEndpoint(t, server.URL), 123, true, 1*time.Millisecond)

		require.NoError(t, err)

		assert.GreaterOrEqual(t, *pollCount, 3)
	})

	t.Run("retries on unknown status", func(t *testing.T) {
		server, pollCount := newPollingTestServer(t, []storage.JobStatus{
			"unknown_status",
			"unknown_status",
			storage.JobStatusDone,
		})

		cmd, _ := newTestCmd(t)
		err := waitForPromptJob(cmd, mustParseEndpoint(t, server.URL), 123, true, 1*time.Millisecond)

		require.NoError(t, err)

		assert.GreaterOrEqual(t, *pollCount, 3)
	})

	t.Run("fails after max unknown retries", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/jobs":
				writeJSON(w, map[string][]storage.ReviewJob{
					"jobs": {{ID: 123, Status: "unknown_status"}},
				})
			}
		}))
		t.Cleanup(server.Close)

		cmd, _ := newTestCmd(t)
		err := waitForPromptJob(cmd, mustParseEndpoint(t, server.URL), 123, true, 1*time.Millisecond)

		require.Error(t, err, "Expected error for max unknown retries")

		assert.Contains(t, err.Error(), "giving up")
	})

	for _, tt := range []struct {
		name     string
		interval time.Duration
	}{
		{"zero", 0},
		{"negative", -1 * time.Millisecond},
	} {
		t.Run("falls back to default interval when pollInterval is "+tt.name, func(t *testing.T) {
			// Override the fallback interval to a known value
			origInterval := promptPollInterval
			promptPollInterval = 5 * time.Millisecond
			t.Cleanup(func() { promptPollInterval = origInterval })

			var pollTimes []time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/jobs":
					pollTimes = append(pollTimes, time.Now())
					status := storage.JobStatusRunning
					if len(pollTimes) >= 3 {
						status = storage.JobStatusDone
					}
					writeJSON(w, map[string][]storage.ReviewJob{
						"jobs": {{ID: 123, Status: status}},
					})
				case "/api/review":
					writeJSON(w, stubReview(123, "test-agent", "Result"))
				}
			}))
			t.Cleanup(server.Close)

			cmd, _ := newTestCmd(t)
			err := waitForPromptJob(cmd, mustParseEndpoint(t, server.URL), 123, true, tt.interval)

			require.NoError(t, err, "Expected no error, got: %v")

			assert.GreaterOrEqual(t, len(pollTimes), 3)
			// Verify polls are spaced by at least 3ms (fallback is 5ms;
			// allow slack for scheduling jitter but reject busy-polling)
			for i := 1; i < len(pollTimes); i++ {
				gap := pollTimes[i].Sub(pollTimes[i-1])
				if gap < 3*time.Millisecond {
					assert.GreaterOrEqual(t, gap, 3*time.Millisecond,
						"Poll gap %d→%d was %v, expected ≥3ms (fallback interval)",
						i-1, i, gap,
					)
				}
			}
		})
	}

	t.Run("resets unknown counter on known status", func(t *testing.T) {
		statuses := []storage.JobStatus{
			"unknown_status", "unknown_status", "unknown_status", "unknown_status", "unknown_status",
			storage.JobStatusRunning,
			"unknown_status", "unknown_status", "unknown_status", "unknown_status", "unknown_status", "unknown_status",
			storage.JobStatusDone,
		}
		server, pollCount := newPollingTestServer(t, statuses)

		cmd, _ := newTestCmd(t)
		err := waitForPromptJob(cmd, mustParseEndpoint(t, server.URL), 123, true, 1*time.Millisecond)

		require.NoError(t, err)

		assert.GreaterOrEqual(t, *pollCount, 13)
	})
}
