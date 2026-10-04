package main

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShowRejectsErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusForbidden, "403 Forbidden"},
		{http.StatusInternalServerError, "500 Internal Server Error"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/review" {
					http.NotFound(w, r)
					return
				}
				assert.Equal(t, "job_id=42", r.URL.RawQuery)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"review unavailable"}`))
			}))
			cmd := showCmd()
			cmd.SetArgs([]string{"--job", "42", "--json"})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)

			err := cmd.Execute()
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "review unavailable")
			assert.Empty(t, output.String())
		})
	}
}
