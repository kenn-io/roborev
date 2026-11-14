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
		canceled     bool
		wantErr      bool
	}{
		{name: "owner", worker: "worker-a"},
		{name: "other worker", worker: "worker-b", wantErr: true},
		{name: "canceled", worker: "worker-a", canceled: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestDB(t)
			defer db.Close()
			_, _, job := createJobChain(t, db, "/tmp/example-repo", "session-clear")
			claimJob(t, db, "worker-a")
			source := uuid.New()
			_, err := db.ExecContext(t.Context(), "UPDATE review_jobs SET session_id='old-session', session_resumed=1, resume_source_job_uuid=? WHERE id=?", source, job.ID)
			require.NoError(t, err)
			if tc.canceled {
				require.NoError(t, db.CancelJob(job.ID))
			}
			err = db.IsolateJobSession(job.ID, tc.worker)
			var isolated bool
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT session_isolated FROM review_jobs WHERE id=?", job.ID).Scan(&isolated))
			if tc.wantErr {
				require.ErrorIs(t, err, sql.ErrNoRows)
				assert.False(t, isolated)
				got, err := db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, "old-session", got.SessionID)
				assert.Equal(t, &source, got.ResumeSourceJobUUID)
				return
			}
			require.NoError(t, err)
			assert.True(t, isolated)
			got, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Empty(t, got.SessionID)
			assert.Nil(t, got.ResumeSourceJobUUID)
			var resumed int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT session_resumed FROM review_jobs WHERE id=?", job.ID).Scan(&resumed))
			assert.Zero(t, resumed)
			require.NoError(t, db.SaveJobSessionID(job.ID, "worker-a", "new-session"))
			got, err = db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, "new-session", got.SessionID)
		})
	}
}

func TestClearJobSessionIDAllowsFreshCapture(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, _, job := createJobChain(t, db, "/tmp/example-repo", "session-resume-fallback")
	claimJob(t, db, "worker-a")
	source := uuid.New()
	_, err := db.ExecContext(t.Context(), `
		UPDATE review_jobs
		SET session_id = 'old-session', session_resumed = 1, resume_source_job_uuid = ?
		WHERE id = ?`, source, job.ID,
	)
	require.NoError(t, err)

	err = db.ClearJobSessionID(job.ID, "worker-b")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, db.ClearJobSessionID(job.ID, "worker-a"))
	var sessionID sql.NullString
	var resumed bool
	var resumeSource sql.NullString
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT session_id, session_resumed, resume_source_job_uuid
		FROM review_jobs WHERE id = ?`, job.ID,
	).Scan(&sessionID, &resumed, &resumeSource))
	assert.False(t, sessionID.Valid)
	assert.False(t, resumed)
	assert.False(t, resumeSource.Valid)

	require.NoError(t, db.SaveJobSessionID(job.ID, "worker-a", "fresh-session"))
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh-session", stored.SessionID)
}

func TestClearJobSessionIDAllowsReplacingFailedAttemptSession(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, _, job := createJobChain(t, db, "/tmp/example-repo", "session-clear-failed-attempt")
	claimJob(t, db, "worker-a")
	_, err := db.ExecContext(t.Context(), `
		UPDATE review_jobs
		SET session_id = 'failed-attempt-session', session_resumed = 0
		WHERE id = ?`, job.ID,
	)
	require.NoError(t, err)

	require.NoError(t, db.ClearJobSessionID(job.ID, "worker-a"))
	cleared, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, cleared.SessionID)
	require.NoError(t, db.SaveJobSessionID(job.ID, "worker-a", "replacement-session"))
	replaced, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "replacement-session", replaced.SessionID)
}
