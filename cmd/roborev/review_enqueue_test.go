package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestDescribeEnqueue(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name        string
		job         storage.ReviewJob
		memberCount int
		dirty       bool
		want        string
	}{
		{
			name: "single commit",
			job:  storage.ReviewJob{ID: 42, GitRef: "abc1234def", Agent: "codex"},
			want: "Enqueued job 42 for abc1234 (agent: codex)",
		},
		{
			name:  "single dirty",
			job:   storage.ReviewJob{ID: 7, GitRef: "dirty", Agent: "codex"},
			dirty: true,
			want:  "Enqueued dirty review job 7 (agent: codex)",
		},
		{
			name:        "panel run",
			job:         storage.ReviewJob{ID: 99, GitRef: "abc1234def", Agent: "codex", PanelName: "branch_final"},
			memberCount: 3,
			want:        "Enqueued job 99 (panel: branch_final, 3 reviewers) for abc1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(tt.want, describeEnqueue(tt.job, tt.memberCount, tt.dirty))
		})
	}
}
