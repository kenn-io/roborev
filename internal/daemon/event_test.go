package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvent_MarshalJSON(t *testing.T) {
	digest := "1234567890abcdef"
	event := Event{
		Type:         "review.completed",
		TS:           time.Date(2026, 1, 11, 10, 0, 30, 0, time.UTC),
		JobID:        42,
		Repo:         "/path/to/myrepo",
		RepoName:     "myrepo",
		SHA:          "abc123",
		Agent:        "claude-code",
		Verdict:      "F",
		Findings:     "Missing input validation",
		ReviewType:   "custom",
		RubricSHA256: &digest,
	}

	data, err := event.MarshalJSON()
	require.NoError(t, err, "MarshalJSON failed")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded), "Could not unmarshal generated JSON")

	tests := []struct {
		key      string
		expected any
	}{
		{"type", "review.completed"},
		{"ts", "2026-01-11T10:00:30Z"},
		{"repo", "/path/to/myrepo"},
		{"repo_name", "myrepo"},
		{"sha", "abc123"},
		{"agent", "claude-code"},
		{"verdict", "F"},
		{"findings", "Missing input validation"},
		{"review_type", "custom"},
		{"rubric_sha256", digest},
	}

	assert.Len(t, decoded, len(tests)+1, // +1 for job_id which is a float
		"expected %d fields in JSON, got %d", len(tests)+1, len(decoded))

	for _, tc := range tests {
		assert.Equal(t, tc.expected, decoded[tc.key],
			"expected %s to be %v, got %v", tc.key, tc.expected, decoded[tc.key])
	}

	// Explicitly check that 'error' is not present
	_, hasError := decoded["error"]
	assert.False(t, hasError, "expected 'error' field to be omitted")

	// WorktreePath omitted when empty
	_, hasWT := decoded["worktree_path"]
	assert.False(t, hasWT, "expected 'worktree_path' to be omitted when empty")
}

func TestEvent_MarshalJSON_WorktreePath(t *testing.T) {
	event := Event{
		Type:         "review.completed",
		TS:           time.Date(2026, 1, 11, 10, 0, 30, 0, time.UTC),
		JobID:        42,
		Repo:         "/path/to/myrepo",
		RepoName:     "myrepo",
		SHA:          "abc123",
		Agent:        "claude-code",
		WorktreePath: "/worktrees/feature-branch",
	}

	data, err := event.MarshalJSON()
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "/worktrees/feature-branch", decoded["worktree_path"])
}
