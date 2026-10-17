package storage

import (
	"bytes"
	"log"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostcap3SyncUnknownOutage(t *testing.T) {
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
	var startErr error
	synctest.Test(t, func(t *testing.T) {
		startErr = worker.Start()
		synctest.Wait()
		worker.Stop()
	})
	// Refuse the unsupported state before starting a run that can replace it.
	assert.ErrorContains(t, startErr, "outage state")
	assert.Contains(t, output.String(), "outage state")
	assert.Equal(t, 0, reports)
	stored, err := db.GetSyncState(syncStateOutage)
	require.NoError(t, err)
	assert.Equal(t, "future-encoding", stored)
}
