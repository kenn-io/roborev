package storage

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestSyncState(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	t.Run("get nonexistent key returns empty", func(t *testing.T) {
		a := assert.New(t)
		r := require.New(t)

		val, err := db.GetSyncState("nonexistent")
		r.NoError(err, "Unexpected error: %v", err)
		a.Empty(val)
	})

	t.Run("set and get", func(t *testing.T) {
		a := assert.New(t)
		r := require.New(t)

		err := db.SetSyncState("test_key", "test_value")
		r.NoError(err, "SetSyncState failed: %v", err)

		val, err := db.GetSyncState("test_key")
		r.NoError(err, "GetSyncState failed: %v", err)
		a.Equal("test_value", val, "Expected 'test_value', got %q", val)
	})

	t.Run("upsert overwrites", func(t *testing.T) {
		a := assert.New(t)
		r := require.New(t)

		err := db.SetSyncState("upsert_key", "first")
		r.NoError(err, "SetSyncState failed: %v", err)

		err = db.SetSyncState("upsert_key", "second")
		r.NoError(err, "SetSyncState upsert failed: %v", err)

		val, err := db.GetSyncState("upsert_key")
		r.NoError(err, "GetSyncState failed: %v", err)
		a.Equal("second", val, "Expected 'second', got %q", val)
	})
}

func TestGetMachineID(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// UUID pattern
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	t.Run("generates new ID on first call", func(t *testing.T) {
		a := assert.New(t)
		r := require.New(t)

		id, err := db.GetMachineID()
		r.NoError(err, "GetMachineID failed: %v", err)
		a.Regexp(uuidPattern, id, "Machine ID %q is not a valid UUID", id)
	})

	t.Run("returns same ID on subsequent calls", func(t *testing.T) {
		a := assert.New(t)
		r := require.New(t)

		id1, err := db.GetMachineID()
		r.NoError(err, "First GetMachineID failed: %v", err)

		id2, err := db.GetMachineID()
		r.NoError(err, "Second GetMachineID failed: %v", err)

		a.Equal(id1, id2, "Machine IDs differ: %q vs %q", id1, id2)
	})
}

func TestGetMachineID_EmptyValueRegeneration(t *testing.T) {
	a := assert.New(t)
	r := require.New(t)

	db := openTestDB(t)
	defer db.Close()

	// Insert an empty machine ID (simulating manual edit or past bug)
	_, err := db.Exec(`INSERT OR REPLACE INTO sync_state (key, value) VALUES (?, '')`, SyncStateMachineID)
	r.NoError(err, "Failed to insert empty machine ID: %v", err)

	// GetMachineID should regenerate when value is empty
	id, err := db.GetMachineID()
	r.NoError(err, "GetMachineID failed: %v", err)
	a.NotEmpty(id, "Expected non-empty machine ID after regeneration")

	// Verify it's now stored
	var stored string
	err = db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, SyncStateMachineID).Scan(&stored)
	r.NoError(err, "Failed to query stored ID: %v", err)
	a.Equal(id, stored, "Stored ID %q doesn't match returned ID %q", stored, id)
}

func TestGetOrCreateSyncStateValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	db := openTestDB(t)
	defer db.Close()

	createCalls := 0
	first, err := db.GetOrCreateSyncStateValue(" test_key ", func() (string, error) {
		createCalls++
		return " first_value ", nil
	})
	require.NoError(err)

	second, err := db.GetOrCreateSyncStateValue("test_key", func() (string, error) {
		createCalls++
		return "second_value", nil
	})
	require.NoError(err)

	assert.Equal("first_value", first)
	assert.Equal(first, second)
	assert.Equal(1, createCalls)
}

func TestSyncWorker_StartStopStart(t *testing.T) {
	// This test verifies that SyncWorker can be started, stopped, and restarted
	// without issues (channel reinitialization on restart).
	db := openTestDB(t)
	defer db.Close()

	// Note: Sync is disabled by default, so Start() will fail with "sync is not enabled"
	// which is expected. We're testing that the worker handles restarts correctly
	// when enabled.

	t.Run("Start fails when sync is disabled", func(t *testing.T) {
		r := require.New(t)

		worker := NewSyncWorker(db, config.SyncConfig{Enabled: false})
		err := worker.Start()
		r.Error(err, "Expected error when starting with sync disabled")
	})

	t.Run("Start fails without postgres_url", func(t *testing.T) {
		r := require.New(t)

		worker := NewSyncWorker(db, config.SyncConfig{
			Enabled:  true,
			Interval: "1s",
		})
		// This will start the goroutine which will fail to connect,
		// but Start() itself should succeed
		err := worker.Start()
		r.NoError(err, "Expected Start to succeed (connection fails async): %v", err)
		// Stop the worker
		worker.Stop()
	})

	t.Run("Start-Stop-Start cycle works", func(t *testing.T) {
		r := require.New(t)

		worker := NewSyncWorker(db, config.SyncConfig{
			Enabled:  true,
			Interval: "1s",
		})

		// First start
		err := worker.Start()
		r.NoError(err, "First Start failed: %v", err)

		// Stop
		worker.Stop()

		// Second start - this tests that channels are reinitialized
		err = worker.Start()
		r.NoError(err, "Second Start failed (channel reinit issue?): %v", err)

		// Stop again
		worker.Stop()
	})

	t.Run("Double start fails", func(t *testing.T) {
		r := require.New(t)

		worker := NewSyncWorker(db, config.SyncConfig{
			Enabled:  true,
			Interval: "1s",
		})

		err := worker.Start()
		r.NoError(err, "First Start failed: %v", err)
		defer worker.Stop()

		err = worker.Start()
		r.Error(err, "Expected error on double start")
	})
}

func TestSyncWorker_SyncNowReturnsErrorWhenNotRunning(t *testing.T) {
	a := assert.New(t)
	r := require.New(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	r.NoError(err, "Failed to open database: %v", err)
	defer db.Close()

	cfg := config.SyncConfig{
		Enabled: true,
	}
	worker := NewSyncWorker(db, cfg)

	// SyncNow should return error when worker is not running
	_, err = worker.SyncNow()
	r.Error(err, "Expected error from SyncNow when worker not running")
	a.Contains(err.Error(), "not running")
}

func TestSyncWorker_FinalPushReturnsNilWhenNotConnected(t *testing.T) {
	r := require.New(t)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	r.NoError(err, "Failed to open database: %v", err)
	defer db.Close()

	cfg := config.SyncConfig{
		Enabled: true,
	}
	worker := NewSyncWorker(db, cfg)

	// FinalPush should return nil when not connected (nothing to push)
	err = worker.FinalPush()
	r.NoError(err, "FinalPush should return nil when not connected, got: %v", err)
}
