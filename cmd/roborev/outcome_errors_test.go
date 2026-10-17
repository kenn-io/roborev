package main

import (
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/storage"
)

func TestOutcomeStateErrorOutput(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	t.Chdir(repo.Dir)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
	result := runCLI(t, "review", "--wait", "--dirty", "--server", md.Server.URL)
	require.ErrorContains(t, result.err, "no uncommitted changes to review")
	assert.Contains(t, result.stderr, "Error: no uncommitted changes to review")
	assert.NotContains(t, result.stdout+result.stderr, "Usage:")
	assert.Empty(t, outcomePosts(rec))
}

func outcomePosts(rec *cliUseRecorder) []recordedTelemetry {
	return slices.DeleteFunc(rec.telemetryPosts(), func(post recordedTelemetry) bool { return post.body == cliUseBody })
}

func TestOutcomeInvalidReasoning(t *testing.T) {
	for _, args := range [][]string{{"fix", "99"}, {"analyze", "refactor", "--fix", "main.go"}} {
		t.Run(args[0], func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			t.Chdir(repo.Dir)
			md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
			job := &storage.ReviewJob{ID: 99, GitRef: "abc123", Agent: "test", Status: storage.JobStatusDone}
			md.State.jobs[99] = job
			md.State.reviews["abc123"] = &storage.Review{ID: 1, JobID: 99, Output: "Finding", Job: job}
			result := runCLI(t, append(slices.Clone(args), "--reasoning", "typo", "--server", md.Server.URL)...)
			require.ErrorContains(t, result.err, "reasoning")
			assert.Empty(t, outcomePosts(rec))
		})
	}
}

func TestOutcomeMultiWaitStoredFailures(t *testing.T) {
	for _, status := range []storage.JobStatus{storage.JobStatusFailed, storage.JobStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			md, rec := newCLIUseDaemon(t, MockRefineHooks{OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
				writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 42, Status: status, Error: "provider unavailable"}, {ID: 43, Status: status, Error: "provider unavailable"}}})
				return true
			}}, nil)
			result := runCLI(t, "wait", "--job", "42", "43", "--server", md.Server.URL)
			requireExitCode(t, result.err, 1)
			assert.Contains(t, result.stdout, "Job 42:")
			assert.Contains(t, result.stdout, "Job 43:")
			assert.Empty(t, outcomePosts(rec))
		})
	}
}
