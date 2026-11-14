package storage

import (
	"database/sql"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsolateJobSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, worker string
		clear        bool
		canceled     bool
		wantErr      bool
		isolate      bool
	}{
		{name: "isolate owner", worker: "worker-a", isolate: true},
		{name: "isolate other worker", worker: "worker-b", isolate: true, wantErr: true},
		{name: "isolate canceled", worker: "worker-a", isolate: true, canceled: true, wantErr: true},
		{name: "clear owner", worker: "worker-a", clear: true},
		{name: "clear other worker", worker: "worker-b", clear: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestDB(t)
			defer db.Close()
			_, _, job := createJobChain(t, db, "/tmp/example-repo", "session-clear")
			claimJob(t, db, "worker-a")
			source := uuid.New()
			_, err := db.ExecContext(t.Context(), `
				UPDATE review_jobs
				SET session_id='old-session', session_cli='agy', session_resumed=1, resume_source_job_uuid=?
				WHERE id=?`, source, job.ID,
			)
			require.NoError(t, err)
			if tc.canceled {
				require.NoError(t, db.CancelJob(job.ID))
			}
			if tc.clear {
				err = db.ClearJobSessionID(job.ID, tc.worker)
			} else {
				err = db.IsolateJobSession(job.ID, tc.worker)
			}
			var isolated bool
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT session_isolated FROM review_jobs WHERE id=?", job.ID).Scan(&isolated))
			if tc.wantErr {
				require.ErrorIs(t, err, sql.ErrNoRows)
				assert.False(t, isolated)
				var sessionID, resumeSource sql.NullString
				var resumed bool
				require.NoError(t, db.QueryRowContext(t.Context(), `
					SELECT session_id, session_resumed, resume_source_job_uuid
					FROM review_jobs WHERE id=?`, job.ID,
				).Scan(&sessionID, &resumed, &resumeSource))
				assert.Equal(t, "old-session", sessionID.String)
				assert.True(t, sessionID.Valid)
				assert.True(t, resumed)
				assert.Equal(t, source.String(), resumeSource.String)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.isolate, isolated)
			var sessionID, sessionCLI, resumeSource sql.NullString
			var resumed bool
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT session_id, session_cli, session_resumed, resume_source_job_uuid
				FROM review_jobs WHERE id=?`, job.ID,
			).Scan(&sessionID, &sessionCLI, &resumed, &resumeSource))
			assert.False(t, sessionID.Valid)
			assert.False(t, sessionCLI.Valid)
			assert.False(t, resumed)
			assert.False(t, resumeSource.Valid)
			require.NoError(t, db.SaveJobSessionIDWithCLI(job.ID, "worker-a", "new-session", "gemini"))
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT session_id, session_cli FROM review_jobs WHERE id = ?`, job.ID,
			).Scan(&sessionID, &sessionCLI))
			assert.Equal(t, "new-session", sessionID.String)
			assert.Equal(t, "gemini", sessionCLI.String)
		})
	}
}
