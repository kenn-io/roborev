package storage

import (
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimSetupFinishedRequiresReadableReview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		fixture            searchFeedFixture
		remote, diff, want bool
	}{
		{name: "done", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review"}, want: true},
		{name: "applied", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusApplied, output: "Review"}, want: true},
		{name: "rebased", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusRebased, output: "Review"}, want: true},
		{name: "remote", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review"}, remote: true, want: true},
		{name: "synthesis", fixture: searchFeedFixture{jobType: JobTypeSynthesis, status: JobStatusDone, structured: string(reviewFixtureJSON("No issues found.")), panelRole: PanelRoleSynthesis}, want: true},
		{name: "legacy commit", fixture: searchFeedFixture{status: JobStatusDone, output: "Review", legacy: true}, want: true},
		{name: "legacy range", fixture: searchFeedFixture{status: JobStatusDone, output: "Review", gitRef: "base..head", noCommit: true, legacy: true}, want: true},
		{name: "legacy dirty", fixture: searchFeedFixture{status: JobStatusDone, output: "Review", gitRef: "dirty", noCommit: true, legacy: true}, want: true},
		{name: "legacy diff", fixture: searchFeedFixture{status: JobStatusDone, output: "Review", gitRef: "snapshot", noCommit: true, legacy: true}, diff: true, want: true},
		{name: "legacy task", fixture: searchFeedFixture{status: JobStatusDone, output: "Task", gitRef: "task", noCommit: true, legacy: true}},
		{name: "panel member", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review", panelRole: PanelRoleMember}},
		{name: "task", fixture: searchFeedFixture{jobType: JobTypeTask, status: JobStatusDone, output: "Task"}},
		{name: "queued", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusQueued, output: "Review"}},
		{name: "canceled", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusCanceled, output: "Review"}},
		{name: "empty output", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			if tc.name == "done" {
				claimed, err := db.ClaimSetupFinished()
				require.NoError(t, err)
				require.False(t, claimed, "an empty install has no review")
			}
			repoID, commitID := seedSearchFeedBase(t, db)
			reviewID := seedSearchFeedReview(t, db, repoID, commitID, 1, tc.fixture)
			machineID, err := db.GetMachineID()
			require.NoError(t, err)
			if tc.remote {
				machineID = uuid.New()
			}
			_, err = db.Exec(`UPDATE review_jobs SET source_machine_id = ? WHERE id = (SELECT job_id FROM reviews WHERE id = ?)`, machineID, reviewID)
			require.NoError(t, err)
			if tc.diff {
				_, err = db.Exec(`INSERT INTO job_content (job_id, diff_content) SELECT job_id, 'diff' FROM reviews WHERE id = ?`, reviewID)
				require.NoError(t, err)
			}
			if tc.name == "done" {
				var wg sync.WaitGroup
				results := make(chan bool, 8)
				for range 8 {
					wg.Go(func() {
						claimed, err := db.ClaimSetupFinished()
						assert.NoError(t, err)
						results <- claimed
					})
				}
				wg.Wait()
				close(results)
				winners := 0
				for claimed := range results {
					if claimed {
						winners++
					}
				}
				assert.Equal(t, 1, winners)
			} else {
				claimed, err := db.ClaimSetupFinished()
				require.NoError(t, err)
				assert.Equal(t, tc.want, claimed)
			}
			marker, err := db.GetSyncState(SyncStateSetupFinished)
			require.NoError(t, err)
			if tc.want {
				assert.Equal(t, "1", marker)
			} else {
				assert.Empty(t, marker)
			}
			claimed, err := db.ClaimSetupFinished()
			require.NoError(t, err)
			assert.False(t, claimed, "replay cannot claim again")
		})
	}
}
