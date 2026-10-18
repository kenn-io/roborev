package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

func TestQueueIntentSupersededAndConsumed(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	stale, err := PublishQueueIntent(t.Context(), true)
	require.NoError(t, err)
	latest, err := PublishQueueIntent(t.Context(), false)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/queue/pause", nil)
	req.Header.Set(QueueIntentHeader, stale)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	release, err := consumeQueueIntent(t.Context(), db)
	require.NoError(t, err)
	release()
	// A delayed CLI acknowledges startup's matching write without republishing it.
	for _, token := range []string{latest, stale} {
		path, want := "/api/queue/unpause", http.StatusOK
		if token == stale {
			path, want = "/api/queue/pause", http.StatusConflict
		}
		req = httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set(QueueIntentHeader, token)
		w = httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		require.Equal(t, want, w.Code, w.Body.String())
	}
	// A TUI unpause during drain replaces a request whose CLI has already left.
	_, err = PublishQueueIntent(t.Context(), true)
	require.NoError(t, err)
	for _, path := range []string{"/api/shutdown", "/api/queue/unpause"} {
		w = httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	paused, err := db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)
	assert.NoFileExists(t, queueIntentPath())
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)
}

func TestQueueIntentAtServerStartup(t *testing.T) {
	for _, scenario := range []string{"pause", "malformed", "unreadable", "unsupported version", "database failure"} {
		t.Run(scenario, func(t *testing.T) {
			testenv.SetDataDir(t)
			server, db, _ := newTestServer(t)
			_, err := PublishQueueIntent(t.Context(), true)
			require.NoError(t, err)
			switch scenario {
			case "malformed":
				require.NoError(t, os.WriteFile(queueIntentPath(), []byte(`{"version":1,"paused":true}`), 0o600))
			case "unsupported version":
				require.NoError(t, os.WriteFile(queueIntentPath(), []byte(`{"version":2,"token":"future-request","paused":{"future":"format"}}`), 0o600))
				require.ErrorContains(t, server.Start(t.Context()), "unsupported queue request version: 2")
				paused, err := db.IsQueuePaused()
				require.NoError(t, err)
				assert.False(t, paused)
				assert.FileExists(t, queueIntentPath())
				return
			case "unreadable":
				require.NoError(t, os.Remove(queueIntentPath()))
				require.NoError(t, os.Mkdir(queueIntentPath(), 0o700))
			case "database failure":
				_, err := db.Exec(`CREATE TRIGGER reject_pause BEFORE INSERT ON daemon_state WHEN NEW.key = 'queue_paused' BEGIN SELECT RAISE(FAIL, 'queue write rejected'); END`)
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				require.ErrorContains(t, server.Start(ctx), "persist queue request")
				started := false
				select {
				case <-server.workerPool.readyCh:
					started = true
				default:
				}
				assert.False(t, started)
				return
			}
			errCh, _ := startServerAndWaitForRuntime(t, server)
			paused, err := db.IsQueuePaused()
			require.NoError(t, err)
			assert.Equal(t, scenario == "pause", paused)
			assert.NoFileExists(t, queueIntentPath())
			stopTestServer(t, server, errCh)
		})
	}
}

func TestQueueIntentFailedTokenlessWriteIsDiscarded(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	_, err := db.Exec(`CREATE TRIGGER reject_pause BEFORE INSERT ON daemon_state WHEN NEW.key = 'queue_paused' BEGIN SELECT RAISE(FAIL, 'queue write rejected'); END`)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/queue/pause", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NoFileExists(t, queueIntentPath())
}
