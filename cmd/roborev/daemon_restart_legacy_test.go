package main

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterruptDaemonWithUpdateAPI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failRenew bool
		want      []string
	}{
		{name: "renewed", want: []string{"prepare:interrupt", "renew", "shutdown"}},
		{name: "renew failure", failRenew: true, want: []string{"prepare:interrupt", "renew", "release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/update/prepare":
					var body struct {
						Policy string `json:"policy"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
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
			err := interruptDaemonWithUpdateAPI(t.Context(), endpoint)
			if tc.failRenew {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, requests)
		})
	}
}
