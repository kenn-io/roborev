package main

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestOutcomeInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"fix", "bogus"}, {"fix", "--batch", "bogus"},
		{"wait", "--job", "bogus"}, {"wait", "--job", "42", "bogus"},
		{"wait", "bogus"}, {"wait", "42", "bogus"}, {"wait", "--sha", "bogus"},
		{"analyze", "refactor", "--fix", "missing.go"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			t.Chdir(repo.Dir)
			md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
			result := runCLI(t, append(slices.Clone(args), "--server", md.Server.URL)...)
			require.Error(t, result.err)
			for _, post := range rec.telemetryPosts() {
				assert.NotContains(t, post.body, `"error_shown"`)
			}
		})
	}
}

func TestOutcomeReviewShowValidation(t *testing.T) {
	for _, args := range [][]string{
		{"review", "--wait", "--branch=bogus", "--base", "HEAD"},
		{"review", "--wait", "--since", "bogus"},
		{"review", "--wait", "--since", "HEAD"},
		{"review", "--wait", "--branch", "--base", "HEAD"},
		{"review", "--wait", "--dirty"},
		{"review", "--wait", "--branch", "--base", "bogus"},
		{"review", "--wait", "--local", "--reasoning", "typo"},
		{"review", "--wait", "--local", "--min-severity", "typo"},
		{"review", "--wait", "--repo", "missing"},
		{"show"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			if args[0] == "show" {
				t.Chdir(t.TempDir())
			} else {
				repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
				t.Chdir(repo.Dir)
			}
			md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
			result := runCLI(t, append(slices.Clone(args), "--server", md.Server.URL)...)
			require.Error(t, result.err)
			assert.NotContains(t, rec.businessPaths(), "POST /api/enqueue")
			for _, post := range rec.telemetryPosts() {
				assert.NotContains(t, post.body, `"error_shown"`)
			}
		})
	}
}

func TestOutcomeAnalyzeStoppedJob(t *testing.T) {
	for _, status := range []storage.JobStatus{storage.JobStatusFailed, storage.JobStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			t.Chdir(repo.Dir)
			md, rec := newCLIUseDaemon(t, MockRefineHooks{
				OnEnqueue: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					w.WriteHeader(http.StatusCreated)
					writeJSON(w, storage.ReviewJob{ID: 42, Status: storage.JobStatusQueued, Agent: "test"})
					return true
				},
				OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 42, Status: status, Error: "analysis unavailable"}}})
					return true
				},
			}, nil)
			result := runCLI(t, "analyze", "refactor", "--fix", "main.go", "--server", md.Server.URL)
			require.ErrorContains(t, result.err, "analysis failed:")
			assert.Contains(t, rec.businessPaths(), "POST /api/enqueue")
			assert.Contains(t, rec.businessPaths(), "GET /api/jobs")
			for _, post := range rec.telemetryPosts() {
				assert.NotContains(t, post.body, `"error_shown"`)
			}
		})
	}
}
