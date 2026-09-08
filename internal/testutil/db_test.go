package testutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenTestDBIsolatesFixtures(t *testing.T) {
	first := OpenTestDB(t)
	_, err := first.GetOrCreateRepo("/test/repo")
	require.NoError(t, err)
	firstID, err := first.GetDatabaseID()
	require.NoError(t, err)
	require.NoError(t, first.Close())

	// A later fixture must use the empty template, not an earlier test's data
	// or database identity, and must outlive the earlier fixture's connection.
	second := OpenTestDB(t)
	var repos int
	require.NoError(t, second.QueryRow("SELECT COUNT(*) FROM repos").Scan(&repos))
	assert.Equal(t, 0, repos)
	secondID, err := second.GetDatabaseID()
	require.NoError(t, err)
	assert.NotEqual(t, firstID, secondID)
	_, err = second.GetOrCreateRepo("/test/repo")
	require.NoError(t, err)
}
