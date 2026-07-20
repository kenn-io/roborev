//go:build postgres

package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration_SingleReviewAndResponseUpserts(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	machineID := uuid.NewString()
	otherMachineID := uuid.NewString()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobUUID := uuid.NewString()
	createTestJob(t, pool.pool, TestJobOpts{
		UUID:            jobUUID,
		RepoID:          repoID,
		CommitID:        commitID,
		SourceMachineID: machineID,
	})

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.pool.Exec(cleanupCtx, `DELETE FROM responses WHERE job_uuid = $1`, jobUUID)
		_, _ = pool.pool.Exec(cleanupCtx, `DELETE FROM reviews WHERE job_uuid = $1`, jobUUID)
		_, _ = pool.pool.Exec(cleanupCtx, `DELETE FROM review_jobs WHERE uuid = $1`, jobUUID)
		_, _ = pool.pool.Exec(cleanupCtx, `DELETE FROM commits WHERE id = $1`, commitID)
		_, _ = pool.pool.Exec(cleanupCtx, `DELETE FROM repos WHERE id = $1`, repoID)
	})

	t.Run("review insert and conflict update", func(t *testing.T) {
		reviewUUID := uuid.NewString()
		createdAt := time.Date(2026, time.July, 18, 12, 0, 0, 123456000, time.UTC)
		review := SyncableReview{
			UUID:               reviewUUID,
			JobUUID:            jobUUID,
			Agent:              "codex",
			Prompt:             "original prompt",
			Output:             "original output",
			UpdatedByMachineID: machineID,
			CreatedAt:          createdAt,
		}
		require.NoError(t, pool.UpsertReview(ctx, review))

		var storedUUID, storedJobUUID, agent, prompt, output, updatedBy string
		var closed bool
		var storedCreatedAt, firstUpdatedAt time.Time
		err := pool.pool.QueryRow(ctx, `
			SELECT uuid, job_uuid, agent, prompt, output, closed,
			       updated_by_machine_id, created_at, updated_at
			FROM reviews WHERE uuid = $1
		`, reviewUUID).Scan(
			&storedUUID, &storedJobUUID, &agent, &prompt, &output, &closed,
			&updatedBy, &storedCreatedAt, &firstUpdatedAt,
		)
		require.NoError(t, err)
		assert.Equal(t, reviewUUID, storedUUID)
		assert.Equal(t, jobUUID, storedJobUUID)
		assert.Equal(t, "codex", agent)
		assert.Equal(t, "original prompt", prompt)
		assert.Equal(t, "original output", output)
		assert.False(t, closed)
		assert.Equal(t, machineID, updatedBy)
		assert.True(t, storedCreatedAt.Equal(createdAt))
		assert.False(t, firstUpdatedAt.IsZero())

		review.Agent = "replacement-agent"
		review.Prompt = "replacement prompt"
		review.Output = "replacement output"
		review.Closed = true
		review.UpdatedByMachineID = otherMachineID
		review.CreatedAt = createdAt.Add(time.Hour)
		require.NoError(t, pool.UpsertReview(ctx, review))

		var secondUpdatedAt time.Time
		err = pool.pool.QueryRow(ctx, `
			SELECT agent, prompt, output, closed, updated_by_machine_id, created_at, updated_at
			FROM reviews WHERE uuid = $1
		`, reviewUUID).Scan(
			&agent, &prompt, &output, &closed, &updatedBy, &storedCreatedAt, &secondUpdatedAt,
		)
		require.NoError(t, err)
		assert.Equal(t, "codex", agent)
		assert.Equal(t, "original prompt", prompt)
		assert.Equal(t, "original output", output)
		assert.True(t, closed)
		assert.Equal(t, otherMachineID, updatedBy)
		assert.True(t, storedCreatedAt.Equal(createdAt))
		assert.False(t, secondUpdatedAt.Before(firstUpdatedAt))
	})

	t.Run("response insert and conflict no-op", func(t *testing.T) {
		responseUUID := uuid.NewString()
		createdAt := time.Date(2026, time.July, 18, 13, 0, 0, 654321000, time.UTC)
		response := SyncableResponse{
			UUID:            responseUUID,
			JobUUID:         jobUUID,
			Responder:       "human",
			Response:        "original response",
			SourceMachineID: machineID,
			CreatedAt:       createdAt,
		}
		require.NoError(t, pool.InsertResponse(ctx, response))

		response.Responder = "replacement"
		response.Response = "replacement response"
		response.SourceMachineID = otherMachineID
		response.CreatedAt = createdAt.Add(time.Hour)
		require.NoError(t, pool.InsertResponse(ctx, response))

		var storedUUID, storedJobUUID, responder, body, sourceMachineID string
		var storedCreatedAt, insertedAt time.Time
		var count int
		err := pool.pool.QueryRow(ctx, `
			SELECT uuid, job_uuid, responder, response, source_machine_id,
			       created_at, inserted_at, COUNT(*) OVER ()
			FROM responses WHERE uuid = $1
		`, responseUUID).Scan(
			&storedUUID, &storedJobUUID, &responder, &body, &sourceMachineID,
			&storedCreatedAt, &insertedAt, &count,
		)
		require.NoError(t, err)
		assert.Equal(t, responseUUID, storedUUID)
		assert.Equal(t, jobUUID, storedJobUUID)
		assert.Equal(t, "human", responder)
		assert.Equal(t, "original response", body)
		assert.Equal(t, machineID, sourceMachineID)
		assert.True(t, storedCreatedAt.Equal(createdAt))
		assert.False(t, insertedAt.IsZero())
		assert.Equal(t, 1, count)
	})
}
