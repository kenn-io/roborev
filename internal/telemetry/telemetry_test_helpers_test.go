package telemetry

import (
	"testing"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	return testutil.OpenTestDB(t)
}
