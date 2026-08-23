package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestNoteClaimErrorSkipsBusy(t *testing.T) {
	el, _ := createTestErrorLog(t)
	wp := &WorkerPool{errorLog: el}

	wp.noteClaimError("worker-1", errors.New("database is locked (5) (SQLITE_BUSY)"))
	assert.Empty(t, el.Recent(), "SQLITE_BUSY must not spam the daemon error log")

	wp.noteClaimError("worker-1", errors.New("disk I/O error"))
	recent := el.Recent()
	require.Len(t, recent, 1)
	assert.Equal(t, "error", recent[0].Level)
	assert.Equal(t, "worker", recent[0].Component)
	assert.Contains(t, recent[0].Message, "claim job:")
	assert.Contains(t, recent[0].Message, "disk I/O error")
}

func TestNoteClaimErrorNilErrorLogBusy(t *testing.T) {
	wp := &WorkerPool{}
	// Must not panic when errorLog is unset (production Start can omit it).
	wp.noteClaimError("worker-1", errors.New("database is locked"))
	wp.noteClaimError("worker-1", errors.New("boom"))
}

func TestIsSQLiteBusyMatchesClaimErrors(t *testing.T) {
	assert.True(t, storage.IsSQLiteBusy(errors.New("database is locked (5) (SQLITE_BUSY)")))
	assert.False(t, storage.IsSQLiteBusy(errors.New("disk I/O error")))
}
