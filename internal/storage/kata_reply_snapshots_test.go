package storage

import (
	"encoding/json/jsontext"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reading a changed thread again during the same run must not replace the
// evidence that run consumed. A later run sees and records the later thread.
func TestKataReplySnapshotIsFrozenPerRun(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "snapshot123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	reviewUUID := reviewUUIDOf(t, env.db, env.job.ID).String() //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	runID := uuid.New().String()                               //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	first := jsontext.Value(`[{"issue_ref":"reviews#abcd","evidence":"First attributed claim"}]`)
	later := jsontext.Value(`[{"issue_ref":"reviews#abcd","evidence":"Edited claim"}]`)

	got, err := env.db.SaveKataReplySnapshot(t.Context(), runID, "review", env.job.ID, reviewUUID, first)
	require.NoError(t, err)
	assert.JSONEq(t, string(first), string(got))
	got, err = env.db.SaveKataReplySnapshot(t.Context(), runID, "review", env.job.ID, reviewUUID, later)
	require.NoError(t, err)
	assert.JSONEq(t, string(first), string(got))

	stored, err := env.db.KataReplySnapshot(t.Context(), runID, env.job.ID, reviewUUID)
	require.NoError(t, err)
	assert.JSONEq(t, string(first), string(stored))
	got, err = env.db.SaveKataReplySnapshot(t.Context(), uuid.New().String(), "fix", env.job.ID, reviewUUID, later) //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	require.NoError(t, err)
	assert.JSONEq(t, string(later), string(got))

	// Rerunning removes the current review, but the consumed original remains.
	require.NoError(t, env.db.ReenqueueJob(env.job.ID, ReenqueueOpts{}))
	stored, err = env.db.KataReplySnapshot(t.Context(), runID, env.job.ID, reviewUUID)
	require.NoError(t, err)
	assert.JSONEq(t, string(first), string(stored))
}

func TestKataReplySnapshotRejectsIncompleteIdentity(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "identity123")
	for _, tc := range []struct {
		run, purpose, review string
	}{
		{"", "fix", uuid.New().String()},               //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
		{uuid.New().String(), "", uuid.New().String()}, //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
		{uuid.New().String(), "fix", ""},               //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	} {
		_, err := env.db.SaveKataReplySnapshot(t.Context(), tc.run, tc.purpose, env.job.ID, tc.review, jsontext.Value(`[]`))
		require.Error(t, err)
	}
}

func TestConcurrentKataReplyConsumersReceiveFirstSnapshot(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "concurrent123")
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	reviewUUID := reviewUUIDOf(t, env.db, env.job.ID).String() //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	runID := uuid.New().String()                               //nolint:forbidigo // Synthetic UUID at the stored or provider text boundary.
	type result struct {
		raw jsontext.Value
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, body := range []jsontext.Value{[]byte(`[{"claim":"first"}]`), []byte(`[{"claim":"second"}]`)} {
		group.Go(func() {
			<-start
			raw, err := env.db.SaveKataReplySnapshot(t.Context(), runID, "fix", env.job.ID, reviewUUID, body)
			results <- result{raw, err}
		})
	}
	close(start)
	group.Wait()
	stored, err := env.db.KataReplySnapshot(t.Context(), runID, env.job.ID, reviewUUID)
	require.NoError(t, err)
	for range 2 {
		got := <-results
		require.NoError(t, got.err)
		assert.JSONEq(t, string(stored), string(got.raw))
	}
}

func TestDeleteRepoRemovesConsumedKataRepliesWithoutForeignKeys(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "delete-replies")
	env.db.SetMaxOpenConns(1)
	_, err := env.db.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	completeWithDocument(t, env.db, env.job.ID, snapshotDocument)
	reviewUUID := reviewUUIDOf(t, env.db, env.job.ID).String() //nolint:forbidigo // Synthetic review UUID at the stored text boundary.
	runID := uuid.New().String()                               //nolint:forbidigo // Synthetic run UUID at the stored text boundary.
	_, err = env.db.SaveKataReplySnapshot(t.Context(), runID, "fix", env.job.ID, reviewUUID, jsontext.Value(`[{"claim":"private discussion"}]`))
	require.NoError(t, err)

	require.NoError(t, env.db.DeleteRepo(env.repo.ID, true))
	stored, err := env.db.KataReplySnapshot(t.Context(), runID, env.job.ID, reviewUUID)
	require.NoError(t, err)
	assert.Empty(t, stored)
}
