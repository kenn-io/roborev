package main

import (
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutcomeRefineInvalidSeverity(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	t.Chdir(repo.Dir)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
	result := runCLI(t, "refine", "--since", repo.HeadSHA(), "--agent", "test", "--min-severity", "typo", "--server", md.Server.URL)
	require.ErrorContains(t, result.err, "min-severity")
	for _, post := range rec.telemetryPosts() {
		assert.NotContains(t, post.body, `"error_shown"`)
	}
}

func TestOutcomeMultiWaitPollingFailure(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	md, rec := newCLIUseDaemon(t, MockRefineHooks{OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
		http.Error(w, "poll unavailable", http.StatusInternalServerError)
		return true
	}}, nil)
	result := runCLI(t, "wait", "--job", "42", "43", "--server", md.Server.URL)
	requireExitCode(t, result.err, 1)
	assert.Contains(t, result.stdout, "Job 42:")
	assert.Contains(t, result.stdout, "Job 43:")
	posts := slices.DeleteFunc(rec.telemetryPosts(), func(post recordedTelemetry) bool { return post.body == cliUseBody })
	require.Len(t, posts, 1)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"review_viewed"}}`, posts[0].body)
}

func TestOutcomeLocalUnknownAgent(t *testing.T) {
	resetCLIUse(t)
	cliOutputIsTerminal = func(io.Writer) bool { return true }
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
	t.Chdir(repo.Dir)
	md, rec := newCLIUseDaemon(t, MockRefineHooks{}, nil)
	result := runCLI(t, "review", "--wait", "--local", "--agent", "bogus", "--server", md.Server.URL)
	require.ErrorContains(t, result.err, "unknown agent")
	for _, post := range rec.telemetryPosts() {
		assert.NotContains(t, post.body, `"error_shown"`)
	}
}
