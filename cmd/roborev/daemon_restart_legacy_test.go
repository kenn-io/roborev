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
		rebuttal  bool
		flipAt    string
		want      []string
	}{
		{name: "renewed", want: []string{"jobs", "ping", "prepare:interrupt", "ping", "renew", "ping", "shutdown"}},
		{name: "renew failure", failRenew: true, want: []string{"jobs", "ping", "prepare:interrupt", "ping", "renew", "ping", "release"}},
		{name: "running rebuttal", rebuttal: true, want: []string{"jobs"}},
		{name: "replacement after jobs read", flipAt: "/api/jobs", want: []string{"jobs", "ping"}},
		{name: "replacement before shutdown", flipAt: "/api/update/renew", want: []string{"jobs", "ping", "prepare:interrupt", "ping", "renew", "ping", "ping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			var jobsQueryStatus, jobsMethod string
			var replacement atomic.Bool
			var prepareCalls, shutdownCalls, releaseCalls atomic.Int32
			endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == tc.flipAt {
					replacement.Store(true)
				}
				switch r.URL.Path {
				case "/api/jobs":
					requests = append(requests, "jobs")
					jobsMethod = r.Method
					jobsQueryStatus = r.URL.Query().Get("status")
					if tc.rebuttal {
						_, _ = io.WriteString(w, `{"jobs":[{"id":7,"status":"running","source":"rebuttal","session_id":"rebuttal-session"}],"has_more":false,"next_cursor":null}`)
					} else if tc.flipAt != "" {
						_, _ = io.WriteString(w, `{"jobs":[],"has_more":false,"next_cursor":null}`)
					} else {
						_, _ = io.WriteString(w, `{"jobs":[{"id":5,"status":"running","source":"post_commit","session_id":"review-session"}],"has_more":false,"next_cursor":null}`)
					}
				case "/api/ping":
					requests = append(requests, "ping")
					pid := os.Getpid()
					if replacement.Load() {
						pid++
					}
					_, _ = io.WriteString(w, fmt.Sprintf(`{"ok":true,"service":"roborev","pid":%d}`, pid))
				case "/api/update/prepare":
					prepareCalls.Add(1)
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
					releaseCalls.Add(1)
					requests = append(requests, "release")
					_, _ = io.WriteString(w, `{"released":true}`)
				case "/api/shutdown":
					shutdownCalls.Add(1)
					requests = append(requests, "shutdown")
					_, _ = io.WriteString(w, `{"ok":true}`)
				default:
					http.NotFound(w, r)
				}
			}))
			err := interruptDaemonWithUpdateAPI(
				t.Context(), &daemon.RuntimeInfo{PID: os.Getpid()}, endpoint,
			)
			if tc.rebuttal {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "rebuttal")
				assert.Equal(t, "running", jobsQueryStatus)
				assert.Equal(t, http.MethodGet, jobsMethod)
			} else if tc.flipAt != "" {
				require.ErrorIs(t, err, daemon.ErrInterruptShutdownUnsupported)
				if tc.flipAt == "/api/jobs" {
					assert.Zero(t, prepareCalls.Load())
				} else {
					assert.EqualValues(t, 1, prepareCalls.Load())
					assert.Zero(t, shutdownCalls.Load())
					assert.Zero(t, releaseCalls.Load())
				}
			} else if tc.failRenew {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, requests)
		})
	}
}
