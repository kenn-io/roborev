package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/roborev/internal/storage"
)

func timePtr(t time.Time) *time.Time { return &t }

func TestBackfillCandidates(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		jobs    []storage.ReviewJob
		wantIDs []int64
	}{
		{
			name:    "empty input",
			jobs:    nil,
			wantIDs: nil,
		},
		{
			name: "single completed job with session",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
			},
			wantIDs: []int64{1},
		},
		{
			name: "skip job that already has token data and cost",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
					TokenUsage: `{"peak_context_tokens":100,"cost_usd":0.12,"has_cost":true}`,
				},
			},
			wantIDs: nil,
		},
		{
			name: "include job with token data but no cost",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
					TokenUsage: `{"peak_context_tokens":100}`,
				},
			},
			wantIDs: []int64{1},
		},
		{
			name: "skip job that already has cost",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
					TokenUsage: `{"peak_context_tokens":100,"cost_usd":0,"has_cost":true}`,
				},
			},
			wantIDs: nil,
		},
		{
			name: "skip job with no session ID",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					StartedAt: timePtr(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "skip queued job",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusQueued,
					SessionID: "s1",
				},
			},
			wantIDs: nil,
		},
		{
			name: "resumed session: two started jobs share session",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
				{
					ID: 2, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "canceled-before-start sibling does not block backfill",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
				{
					ID: 2, Status: storage.JobStatusCanceled,
					SessionID: "s1", StartedAt: nil,
				},
			},
			wantIDs: []int64{1},
		},
		{
			name: "canceled-after-start sibling blocks backfill",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
				{
					ID: 2, Status: storage.JobStatusCanceled,
					SessionID: "s1", StartedAt: timePtr(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "failed-after-start sibling blocks backfill",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
				{
					ID: 2, Status: storage.JobStatusFailed,
					SessionID: "s1", StartedAt: timePtr(now),
				},
			},
			wantIDs: nil,
		},
		{
			name: "independent sessions are both eligible",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusDone,
					SessionID: "s1", StartedAt: timePtr(now),
				},
				{
					ID: 2, Status: storage.JobStatusDone,
					SessionID: "s2", StartedAt: timePtr(now),
				},
			},
			wantIDs: []int64{1, 2},
		},
		{
			name: "applied/rebased jobs are eligible",
			jobs: []storage.ReviewJob{
				{
					ID: 1, Status: storage.JobStatusApplied,
					SessionID: "s1", StartedAt: timePtr(now),
				},
				{
					ID: 2, Status: storage.JobStatusRebased,
					SessionID: "s2", StartedAt: timePtr(now),
				},
			},
			wantIDs: []int64{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := backfillCandidates(tt.jobs)
			var gotIDs []int64
			for _, j := range got {
				gotIDs = append(gotIDs, j.ID)
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}
