package storage

import (
	"database/sql"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClearJobSession(t *testing.T) {
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
			_, err := db.Exec("UPDATE review_jobs SET session_id='old-session', session_resumed=1, resume_source_job_uuid=? WHERE id=?", source, job.ID)
			require.NoError(t, err)
			if tc.canceled {
				require.NoError(t, db.CancelJob(job.ID))
			}
			err = db.ClearJobSession(job.ID, tc.worker)
			if tc.wantErr {
				require.ErrorIs(t, err, sql.ErrNoRows)
				got, err := db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, "old-session", got.SessionID)
				assert.Equal(t, &source, got.ResumeSourceJobUUID)
				return
			}
			require.NoError(t, err)
			got, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Empty(t, got.SessionID)
			assert.Nil(t, got.ResumeSourceJobUUID)
			var resumed int
			require.NoError(t, db.QueryRow("SELECT session_resumed FROM review_jobs WHERE id=?", job.ID).Scan(&resumed))
			assert.Zero(t, resumed)
			require.NoError(t, db.SaveJobSessionID(job.ID, "worker-a", "new-session"))
			got, err = db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, "new-session", got.SessionID)
		})
	}
}
