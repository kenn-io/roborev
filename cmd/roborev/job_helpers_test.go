package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func TestWaitForJobCompletionReturnsNotFoundImmediately(t *testing.T) {
	var jobCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			jobCalls++
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	review, err := waitForJobCompletion(ctx, server.URL, 123, nil)
	require.Error(t, err)
	assert.Nil(t, review)
	require.ErrorIs(t, err, ErrJobNotFound)
	assert.Equal(t, 1, jobCalls, "expected not-found to fail fast instead of polling until timeout")
}

func TestAuthJobPollingStopsOnTerminalDenial(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       storage.JobStatus
		brokenConfig bool
		calls        int
	}{
		{"job denied", "", false, 1},
		{"partial review denied", storage.JobStatusRunning, false, 2},
		{"finished review denied", storage.JobStatusDone, false, 2},
		{"invalid config", "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			if tc.brokenConfig {
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "invalid key"`), 0o600))
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/api/jobs" && tc.status != "" {
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 23, Status: tc.status}}})
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			review, err := waitForJobCompletion(ctx, server.URL, 23, io.Discard)
			wantErr := daemon.ErrDaemonAccessDenied
			if tc.brokenConfig {
				wantErr = daemon.ErrClientConfig
			}
			require.ErrorIs(t, err, wantErr)
			assert.Nil(t, review)
			assert.Equal(t, tc.calls, calls)
		})
	}
}
