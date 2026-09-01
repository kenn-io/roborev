package agenthook

import (
	"os"
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

func TestTryGrantFixSessionBlocksActiveOwner(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	lineage := "/repo\x00main"
	fixSessions := map[string]FixSession{}

	first, granted := tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-a"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now, firstFixSessionID,
	)
	require.True(t, granted)
	assert.Equal(t, now.Add(12*time.Hour), first.ExpiresAt)

	_, granted = tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-b"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now.Add(time.Hour), secondFixSessionID,
	)
	assert.False(t, granted)
	assert.Equal(t, firstFixSessionID, fixSessions[lineage].ID)
}

func TestTryGrantFixSessionDoesNotRegrantOrRenewOwner(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	lineage := "/repo\x00main"
	fixSessions := map[string]FixSession{}

	first, granted := tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-a"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now, firstFixSessionID,
	)
	require.True(t, granted)

	_, granted = tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-a"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now.Add(11*time.Hour), secondFixSessionID,
	)
	assert.False(t, granted)
	assert.Equal(t, first.ExpiresAt, fixSessions[lineage].ExpiresAt)
}

func TestTryGrantFixSessionReplacesOwnerAtExactExpiry(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	lineage := "/repo\x00main"
	fixSessions := map[string]FixSession{}
	_, granted := tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-a"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now, firstFixSessionID,
	)
	require.True(t, granted)

	replacement, granted := tryGrantFixSession(
		fixSessions,
		Request{Agent: "codex", Event: Input{SessionID: "session-b"}},
		hookScope{WorktreeRoot: "/other-worktree", Branch: "main"},
		lineage, now.Add(FixSessionLifetime), secondFixSessionID,
	)
	require.True(t, granted)
	assert.Equal(t, secondFixSessionID, replacement.ID)
	assert.Equal(t, "codex", replacement.Agent)
	assert.Equal(t, "/other-worktree", replacement.WorktreeRoot)
}

func TestCompleteFixSessionReleasesOwnershipAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
		fixSessions: map[string]FixSession{
			"/repo\x00main": {
				ID: firstFixSessionID, Agent: "claude", SessionID: "session-a",
				StartedAt: now, ExpiresAt: now.Add(FixSessionLifetime),
			},
		},
		now: func() time.Time { return now.Add(time.Hour) },
	}

	completed, err := store.CompleteFixSession(firstFixSessionID)
	require.NoError(t, err)
	assert.Equal(t, now.Add(time.Hour), completed.CompletedAt)

	repeated, err := store.CompleteFixSession(firstFixSessionID)
	require.NoError(t, err)
	assert.Equal(t, completed, repeated)

	_, granted := tryGrantFixSession(
		store.FixSessions(),
		Request{Agent: "codex", Event: Input{SessionID: "session-b"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		"/repo\x00main", now.Add(2*time.Hour), secondFixSessionID,
	)
	assert.True(t, granted)
}

func TestCompleteFixSessionRejectsUnknownAndSupersededIDs(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := &StateStore{
		path:     filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{},
		fixSessions: map[string]FixSession{
			"/repo\x00main": {
				ID: secondFixSessionID, Agent: "codex", SessionID: "session-b",
				StartedAt: now, ExpiresAt: now.Add(FixSessionLifetime),
			},
		},
		now: func() time.Time { return now },
	}

	_, err := store.CompleteFixSession(firstFixSessionID)
	require.ErrorIs(t, err, ErrFixSessionNotFound)
	assert.Zero(t, store.FixSessions()["/repo\x00main"].CompletedAt)
}

func TestFixSessionsReturnsIsolatedSnapshot(t *testing.T) {
	store := &StateStore{
		fixSessions: map[string]FixSession{
			"lineage": {ID: firstFixSessionID, Agent: "claude"},
		},
	}

	got := store.FixSessions()
	got["lineage"] = FixSession{ID: secondFixSessionID, Agent: "codex"}
	delete(got, "lineage")

	assert.Equal(t, firstFixSessionID, store.FixSessions()["lineage"].ID)
}

func TestLoadStateRestoresFixSessions(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.MkdirAll(filepath.Dir(StatePath()), 0o700))
	require.NoError(t, os.WriteFile(StatePath(), []byte(`{
  "sessions": {},
  "fix_sessions": {
    "/repo\u0000main": {
      "id": "00000000-0000-4000-8000-000000000001",
      "agent": "claude",
      "session_id": "session-a",
      "worktree_root": "/worktree",
      "branch": "main",
      "started_at": "2026-08-31T12:00:00Z",
      "expires_at": "2026-09-01T00:00:00Z"
    }
  }
}`), 0o600))

	store, err := LoadState(nil)
	require.NoError(t, err)
	got := store.FixSessions()["/repo\x00main"]
	assert.Equal(t, firstFixSessionID, got.ID)
	assert.Equal(t, now.Add(FixSessionLifetime), got.ExpiresAt)
}

func TestStateStoreResetRemovesOwnedFixSession(t *testing.T) {
	store := &StateStore{
		path: filepath.Join(t.TempDir(), "state.json"),
		sessions: map[string]SessionState{
			"session-a": {Count: 1},
			"session-b": {Count: 2},
		},
		fixSessions: map[string]FixSession{
			"lineage-a": {ID: firstFixSessionID, SessionID: "session-a"},
			"lineage-b": {ID: secondFixSessionID, SessionID: "session-b"},
		},
	}

	require.NoError(t, store.Reset("session-a", false))
	assert.NotContains(t, store.Sessions(), "session-a")
	assert.NotContains(t, store.FixSessions(), "lineage-a")
	assert.Contains(t, store.FixSessions(), "lineage-b")
}

func TestStateStoreResetAllRemovesAllFixSessions(t *testing.T) {
	store := &StateStore{
		path:        filepath.Join(t.TempDir(), "state.json"),
		sessions:    map[string]SessionState{"session-a": {Count: 1}},
		fixSessions: map[string]FixSession{"lineage": {ID: firstFixSessionID}},
	}

	require.NoError(t, store.Reset("", true))
	assert.Empty(t, store.Sessions())
	assert.Empty(t, store.FixSessions())
}

func TestSaveSessionAndFixSessionsRollsBackBothMaps(t *testing.T) {
	store := &StateStore{
		path:        t.TempDir(),
		sessions:    map[string]SessionState{"session-a": {Count: 1}},
		fixSessions: map[string]FixSession{"lineage": {ID: firstFixSessionID}},
	}
	newFixSessions := map[string]FixSession{"lineage": {ID: secondFixSessionID}}

	store.mu.Lock()
	err := store.saveSessionAndFixSessionsLocked("session-a", SessionState{Count: 2}, newFixSessions)
	store.mu.Unlock()

	require.Error(t, err)
	assert.Equal(t, 1, store.Sessions()["session-a"].Count)
	assert.Equal(t, firstFixSessionID, store.FixSessions()["lineage"].ID)
	assert.NotErrorIs(t, err, ErrFixSessionNotFound)
}
