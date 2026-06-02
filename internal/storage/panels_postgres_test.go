//go:build postgres

package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresPanelColumnRoundTrip(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()
	pool := openTestPgPool(t)

	repoID, err := pool.GetOrCreateRepo(ctx, "panel-sync-test-identity")
	require.NoError(t, err)

	job := SyncableJob{
		UUID:                  GenerateUUID(),
		Agent:                 "test",
		Reasoning:             "thorough",
		JobType:               JobTypeReview,
		ReviewType:            "security",
		Status:                "done",
		EnqueuedAt:            time.Now(),
		UpdatedAt:             time.Now(),
		GitRef:                "abc..def",
		SourceMachineID:       GenerateUUID(),
		PanelRunUUID:          "pg-run-1",
		PanelRole:             "member",
		PanelName:             "branch_final",
		PanelMemberName:       "security",
		PanelMemberIndex:      2,
		PanelMemberConfigJSON: `{"agent":"test","review_type":"security"}`,
	}
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))

	// Pull it back from a different machine ID to bypass the echo filter.
	pulled, _, err := pool.PullJobs(ctx, GenerateUUID(), "", 100)
	require.NoError(t, err)
	var found *PulledJob
	for i := range pulled {
		if pulled[i].UUID == job.UUID {
			found = &pulled[i]
			break
		}
	}
	require.NotNil(t, found, "job not pulled back")
	assert.Equal("pg-run-1", found.PanelRunUUID)
	assert.Equal("member", found.PanelRole)
	assert.Equal("branch_final", found.PanelName)
	assert.Equal("security", found.PanelMemberName)
	assert.Equal(2, found.PanelMemberIndex)
	assert.Equal(`{"agent":"test","review_type":"security"}`, found.PanelMemberConfigJSON)
}

func TestPostgresBackupColumnRoundTrip(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()
	pool := openTestPgPool(t)

	repoID, err := pool.GetOrCreateRepo(ctx, "backup-sync-test-identity")
	require.NoError(t, err)

	job := SyncableJob{
		UUID:            GenerateUUID(),
		Agent:           "test",
		Reasoning:       "thorough",
		JobType:         JobTypeReview,
		Status:          "done",
		EnqueuedAt:      time.Now(),
		UpdatedAt:       time.Now(),
		GitRef:          "abc..def",
		SourceMachineID: GenerateUUID(),
		BackupAgent:     "claude-code",
		BackupModel:     "opus",
	}
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))

	// Pull it back from a different machine ID to bypass the echo filter.
	pulled, _, err := pool.PullJobs(ctx, GenerateUUID(), "", 100)
	require.NoError(t, err)
	var found *PulledJob
	for i := range pulled {
		if pulled[i].UUID == job.UUID {
			found = &pulled[i]
			break
		}
	}
	require.NotNil(t, found, "job not pulled back")
	assert.Equal("claude-code", found.BackupAgent)
	assert.Equal("opus", found.BackupModel)
}
