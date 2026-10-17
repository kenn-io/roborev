package storage

import (
	"bytes"
	"log"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutcomeSyncUnknownOutage(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	require.NoError(t, db.SetSyncState(syncStateOutage, "future-encoding"))
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	worker := NewSyncWorker(db, testSyncConfig())
	reports := 0
	worker.SetAfterFailure(func(string) { reports++ })
	synctest.Test(t, func(t *testing.T) {
		// Sync still starts; an unknown stored value only turns off outage reporting.
		require.NoError(t, worker.Start())
		defer worker.Stop()
		synctest.Wait()
		for range 2 {
			_, err := worker.SyncNow()
			require.ErrorContains(t, err, "failed to connect")
		}
	})
	assert.Contains(t, output.String(), "outage state")
	assert.Equal(t, 0, reports)
	stored, err := db.GetSyncState(syncStateOutage)
	require.NoError(t, err)
	assert.Equal(t, "future-encoding", stored)
}
