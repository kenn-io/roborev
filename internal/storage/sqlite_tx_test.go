package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBeginImmediateFailureDoesNotPoolOpenTransaction(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	db.SetMaxOpenConns(1)

	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	// Stand in for a BEGIN that SQLite started before cancellation surfaced.
	_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, beginImmediate(canceled, conn))
	_ = conn.Close()

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Nil(t, claimed)
}
