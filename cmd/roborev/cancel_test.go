package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mockCancelHandler(t *testing.T, receivedJobID *int64, statusCode int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/job/cancel" && r.Method == "POST" {
			var req struct {
				JobID int64 `json:"job_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				assert.NoError(t, err, "failed to decode request body: %v", err)
				return
			}
			if receivedJobID != nil {
				*receivedJobID = req.JobID
			}
			w.WriteHeader(statusCode)
			if statusCode != http.StatusOK {
				_, _ = w.Write([]byte("job not found or not cancellable"))
			}
			return
		}
	}
}

func TestCancelCmd(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		statusCode    int
		wantJobID     int64
		wantErr       bool
		wantErrSubstr string
	}{
		{
			name:       "cancels queued job",
			args:       []string{"42"},
			statusCode: http.StatusOK,
			wantJobID:  42,
		},
		{
			name:          "rejects non-numeric job id",
			args:          []string{"abc"},
			wantErr:       true,
			wantErrSubstr: "invalid job_id",
		},
		{
			name:          "rejects non-positive job id",
			args:          []string{"0"},
			wantErr:       true,
			wantErrSubstr: "invalid job_id",
		},
		{
			name:          "surfaces not-cancellable error",
			args:          []string{"42"},
			statusCode:    http.StatusNotFound,
			wantErr:       true,
			wantErrSubstr: "not cancellable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedJobID int64
			daemonFromHandler(t, mockCancelHandler(t, &receivedJobID, tt.statusCode))

			cmd := cancelCmd()
			cmd.SetArgs(tt.args)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true

			err := cmd.Execute()

			if tt.wantErr {
				require.Error(t, err, "expected error, got nil")
				assert.ErrorContains(t, err, tt.wantErrSubstr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantJobID, receivedJobID, "expected job_id %d, got %d", tt.wantJobID, receivedJobID)
			}
		})
	}
}
