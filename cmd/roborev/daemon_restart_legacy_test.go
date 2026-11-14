package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestInterruptDaemonWithUpdateAPI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failRenew bool
		want      []string
	}{
		{name: "renewed", want: []string{"jobs", "ping", "prepare:interrupt", "ping", "renew", "ping", "shutdown"}},
		{name: "renew failure", failRenew: true, want: []string{"jobs", "ping", "prepare:interrupt", "ping", "renew", "ping", "release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/jobs":
					requests = append(requests, "jobs")
					_, _ = io.WriteString(w, `{"jobs":[{"id":5,"status":"running","source":"post_commit","session_id":"review-session"}],"has_more":false,"next_cursor":null}`)
				case "/api/ping":
					requests = append(requests, "ping")
					_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"service":"roborev","pid":%d}`, os.Getpid()))
				case "/api/update/prepare":
					var body struct {
						Policy string `json:"policy"`
					}
					assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					requests = append(requests, "prepare:"+body.Policy)
					_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
				case "/api/update/renew":
					requests = append(requests, "renew")
					if tc.failRenew {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = io.WriteString(w, `{"title":"Unavailable","status":503}`)
					} else {
						_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
					}
				case "/api/update/release":
					requests = append(requests, "release")
					_, _ = io.WriteString(w, `{"released":true}`)
				case "/api/shutdown":
					requests = append(requests, "shutdown")
					_, _ = io.WriteString(w, `{"ok":true}`)
				default:
					http.NotFound(w, r)
				}
			}))
			err := interruptDaemonWithUpdateAPI(
				t.Context(), &daemon.RuntimeInfo{PID: os.Getpid()}, endpoint,
			)
			if tc.failRenew {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, requests)
		})
	}
}

func TestInterruptDaemonWithUpdateAPIRejectsRunningRebuttalBeforePrepare(t *testing.T) {
	sessionID := "rebuttal-session"
	var requests []string
	var jobsQueryStatus string
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/jobs":
			requests = append(requests, r.Method+" "+r.URL.Path)
			jobsQueryStatus = r.URL.Query().Get("status")
			_, _ = io.WriteString(w, `{"jobs":[{"id":7,"status":"running","source":"rebuttal","session_id":"rebuttal-session"}],"has_more":false,"next_cursor":null}`)
		case "/api/update/prepare":
			requests = append(requests, r.Method+" "+r.URL.Path)
			// The v0.75.0 update API clears this identifier when it requeues a job.
			sessionID = ""
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			requests = append(requests, r.Method+" "+r.URL.Path)
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/shutdown":
			requests = append(requests, r.Method+" "+r.URL.Path)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))

	err := interruptDaemonWithUpdateAPI(t.Context(), &daemon.RuntimeInfo{PID: os.Getpid()}, endpoint)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rebuttal")
	assert.Equal(t, "rebuttal-session", sessionID)
	assert.Equal(t, "running", jobsQueryStatus)
	assert.Equal(t, []string{"GET /api/jobs"}, requests)
}

func TestInterruptDaemonWithUpdateAPIRejectsReplacementAfterJobsRead(t *testing.T) {
	expectedPID := os.Getpid()
	var replacement atomic.Bool
	var prepareCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/jobs":
			replacement.Store(true)
			_, _ = io.WriteString(w, `{"jobs":[],"has_more":false,"next_cursor":null}`)
		case "/api/ping":
			pid := expectedPID
			if replacement.Load() {
				pid++
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"service":"roborev","pid":%d}`, pid))
		case "/api/update/prepare":
			prepareCalls.Add(1)
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/update/renew":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/shutdown":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))

	err := interruptDaemonWithUpdateAPI(t.Context(), &daemon.RuntimeInfo{PID: expectedPID}, endpoint)

	require.ErrorIs(t, err, daemon.ErrInterruptShutdownUnsupported)
	assert.Zero(t, prepareCalls.Load())
}

func TestInterruptDaemonWithUpdateAPIRejectsReplacementBeforeShutdown(t *testing.T) {
	expectedPID := os.Getpid()
	var replacement atomic.Bool
	var shutdownCalls atomic.Int32
	var releaseCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/jobs":
			_, _ = io.WriteString(w, `{"jobs":[],"has_more":false,"next_cursor":null}`)
		case "/api/ping":
			pid := expectedPID
			if replacement.Load() {
				pid++
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"service":"roborev","pid":%d}`, pid))
		case "/api/update/prepare":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			replacement.Store(true)
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/update/release":
			releaseCalls.Add(1)
			_, _ = io.WriteString(w, `{"released":true}`)
		case "/api/shutdown":
			shutdownCalls.Add(1)
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))

	err := interruptDaemonWithUpdateAPI(t.Context(), &daemon.RuntimeInfo{PID: expectedPID}, endpoint)

	require.ErrorIs(t, err, daemon.ErrInterruptShutdownUnsupported)
	assert.Zero(t, shutdownCalls.Load())
	assert.Zero(t, releaseCalls.Load())
}
