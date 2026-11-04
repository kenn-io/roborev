package storage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func queryPlan(t *testing.T, q querier, query string, args ...any) string {
	t.Helper()
	rows, err := q.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(details, "\n")
}
