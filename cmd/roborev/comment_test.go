package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func mockCommentHandler(t *testing.T, receivedJobID *int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/comment" && r.Method == "POST" {
			var req struct {
				JobID int64 `json:"job_id"`
			}
			decoder := json.NewDecoder(r.Body)
			if err := decoder.Decode(&req); err != nil {
				assert.NoError(t, err, "failed to decode request body: %v", err)
				return
			}
			*receivedJobID = req.JobID
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(storage.Response{ID: 1, JobID: receivedJobID}); err != nil {
				assert.NoError(t, err, "failed to encode response: %v", err)
			}
			return
		}
	}
}

func TestCommentJobFlag(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantJobID     int64
		wantErr       bool
		wantErrSubstr string
	}{
		{
			name:      "forces job ID interpretation",
			args:      []string{"--job", "1234567", "-m", "test message"},
			wantJobID: 1234567,
			wantErr:   false,
		},
		{
			name:          "rejects non-numeric input",
			args:          []string{"--job", "abc123", "-m", "test"},
			wantErr:       true,
			wantErrSubstr: "--job requires numeric job ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedJobID int64

			if !tt.wantErr {
				daemonFromHandler(t, mockCommentHandler(t, &receivedJobID))
			} else {
				_ = mockCommentHandler(t, nil)
			}

			cmd := commentCmd()
			cmd.SetArgs(tt.args)

			if tt.wantErr {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
			}
			err := cmd.Execute()

			if tt.wantErr {
				require.Error(t, err, "expected error, got nil")
				require.ErrorContains(t, err, tt.wantErrSubstr, "expected error containing %q", tt.wantErrSubstr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.wantJobID, receivedJobID, "expected job_id %d, got %d", tt.wantJobID, receivedJobID)

			}
		})
	}
}
