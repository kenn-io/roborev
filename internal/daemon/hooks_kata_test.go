package daemon

import (
	"log"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

func TestKataCreateRequestFailed(t *testing.T) {
	hook := config.HookConfig{Type: "kata"}
	event := Event{Type: "review.failed", JobID: 7, RepoName: "myrepo", SHA: "abc123def456", Agent: "codex", Error: "boom", TS: time.Unix(1700000000, 0).UTC()}

	req, ok := kataCreateRequest(hook, event)
	require.True(t, ok)
	assert.Contains(t, req.Title, "Review failed for myrepo")
	assert.Contains(t, req.Title, "roborev show 7")
	require.NotNil(t, req.Priority)
	assert.Equal(t, 1, *req.Priority)
	assert.Contains(t, req.Labels, "roborev")
	assert.Contains(t, req.Labels, "review-failed")
	assert.Equal(t, "roborev:7:review.failed:abc123def456", req.IdempotencyKey)
	assert.Contains(t, req.Body, "boom")
	assert.Contains(t, req.Body, "roborev show 7")
}

func TestKataCreateRequestPrefersUUID(t *testing.T) {
	hook := config.HookConfig{Type: "kata"}
	event := Event{Type: "review.failed", JobID: 7, JobUUID: "uuid-abc", SHA: "deadbeef"}
	req, ok := kataCreateRequest(hook, event)
	require.True(t, ok)
	assert.Equal(t, "roborev:job:uuid-abc:review.failed", req.IdempotencyKey)
}

func TestKataCreateRequestFindings(t *testing.T) {
	p := 4
	hook := config.HookConfig{Type: "kata", Project: "proj", Labels: []string{"team"}, Priority: &p}
	event := Event{Type: "review.completed", Verdict: "F", JobID: 9, RepoName: "r", SHA: "deadbeef", Findings: "## Bug\nbad"}

	req, ok := kataCreateRequest(hook, event)
	require.True(t, ok)
	assert.Equal(t, "proj", req.Project)
	assert.Equal(t, 4, *req.Priority) // configured priority wins
	assert.Contains(t, req.Labels, "team")
	assert.Contains(t, req.Labels, "review-finding")
	assert.Contains(t, req.Body, "roborev fix 9")
	assert.Contains(t, req.Body, "bad")
	assert.Contains(t, req.Body, "\n\nFix:")
}

func TestKataCreateRequestPassingSkipped(t *testing.T) {
	_, ok := kataCreateRequest(config.HookConfig{Type: "kata"}, Event{Type: "review.completed", Verdict: "P", JobID: 1})
	assert.False(t, ok)
}

func TestKataHookBodyTruncated(t *testing.T) {
	event := Event{Type: "review.completed", Verdict: "F", JobID: 2, SHA: "s", Findings: strings.Repeat("x", kataHookMaxBodyBytes*2)}
	req, ok := kataCreateRequest(config.HookConfig{Type: "kata"}, event)
	require.True(t, ok)
	assert.LessOrEqual(t, len(req.Body), kataHookMaxBodyBytes+64)
	assert.Contains(t, req.Body, "truncated")
}

func TestRunKataHookCreatesAndLogs(t *testing.T) {
	var buf strings.Builder
	fake := &katatest.FakeClient{CreateResult: kata.CreateResult{ShortID: "new1"}}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata", Project: "p"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())

	require.Len(t, fake.CreateReqs, 1)
	assert.Contains(t, buf.String(), "created issue new1")
}

func TestRunKataHookLogsNoBinding(t *testing.T) {
	var buf strings.Builder
	fake := &katatest.FakeClient{BindingErr: kata.ErrNoBinding}
	hr := &HookRunner{
		logger:        log.New(&buf, "", 0),
		newKataClient: func(string) kata.Client { return fake },
	}
	hr.wg.Add(1)
	hr.runKataHook(config.HookConfig{Type: "kata"}, Event{Type: "review.failed", JobID: 5, SHA: "x"}, t.TempDir())

	assert.Empty(t, fake.CreateReqs, "must not create without a binding")
	assert.Contains(t, buf.String(), "no .kata.toml binding")
}
