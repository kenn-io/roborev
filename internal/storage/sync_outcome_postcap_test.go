package storage_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/testutil"
)

func TestOutcomeUnknownSyncStatePostsFailure(t *testing.T) {
	db := testutil.OpenTestDB(t)
	require.NoError(t, db.SetSyncState("telemetry.sync_outage", "unexpected"))
	var mu sync.Mutex
	var posts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		mu.Lock()
		posts = append(posts, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	for range 2 {
		worker := storage.NewSyncWorker(db, config.SyncConfig{Enabled: true, Interval: "1h"})
		worker.SetAfterFailure(func(reason string) {
			telemetry.PostReviewEvent(t.Context(), server.Client(), server.URL, "sync_failed", "daemon", map[string]any{"reason": reason})
		})
		require.NoError(t, worker.Start())
		for range 2 {
			_, err := worker.SyncNow()
			require.ErrorContains(t, err, "failed to connect")
		}
		worker.Stop()
	}
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, posts, 1)
	assert.JSONEq(t, `{"event":"sync_failed","properties":{"surface":"daemon","reason":"connect"}}`, posts[0])
}
