package agenthook

import (
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	firstFixSessionID  = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	secondFixSessionID = uuid.MustParse("00000000-0000-4000-8000-000000000002")
)

func TestCompleteFixSessionIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := &StateStore{
		path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{},
		fixSessions: map[string]FixSession{"worktree": {
			ID: firstFixSessionID, ExpiresAt: now.Add(FixSessionLifetime),
		}},
		now: func() time.Time { return now },
	}

	completed, err := store.CompleteFixSession(firstFixSessionID)
	require.NoError(t, err)
	assert.Equal(t, now, completed.CompletedAt)
	repeated, err := store.CompleteFixSession(firstFixSessionID)
	require.NoError(t, err)
	assert.Equal(t, completed, repeated)

	_, err = store.CompleteFixSession(secondFixSessionID)
	require.ErrorIs(t, err, ErrFixSessionNotFound)
}
