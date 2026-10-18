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
		name         string
		fixture      searchFeedFixture
		remote, want bool
	}{
		{name: "done", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review"}, want: true},
		{name: "remote", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review"}, remote: true, want: true},
		{name: "panel member", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review", panelRole: PanelRoleMember}, want: true},
		{name: "queued", fixture: searchFeedFixture{jobType: JobTypeReview, status: JobStatusQueued, output: "Review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			if tc.name == "done" {
				claimed, err := db.ClaimSetupFinished(t.Context())
				require.NoError(t, err)
				require.Equal(t, SetupFinishedIneligible, claimed, "an empty install has no review")
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
			if tc.name == "done" {
				var wg sync.WaitGroup
				results := make(chan SetupFinishedClaim, 8)
				for range 8 {
					wg.Go(func() {
						claimed, err := db.ClaimSetupFinished(t.Context())
						assert.NoError(t, err)
						results <- claimed
					})
				}
				wg.Wait()
				close(results)
				winners := 0
				for claimed := range results {
					if claimed == SetupFinishedNewClaim {
						winners++
					}
				}
				assert.Equal(t, 1, winners)
			} else {
				claimed, err := db.ClaimSetupFinished(t.Context())
				require.NoError(t, err)
				assert.Equal(t, tc.want, claimed == SetupFinishedNewClaim)
			}
			marker, err := db.GetSyncState(SyncStateSetupFinished)
			require.NoError(t, err)
			if tc.want {
				assert.Equal(t, "1", marker)
			} else {
				assert.Empty(t, marker)
			}
			claimed, err := db.ClaimSetupFinished(t.Context())
			require.NoError(t, err)
			if tc.want {
				assert.Equal(t, SetupFinishedExisting, claimed)
			} else {
				assert.Equal(t, SetupFinishedIneligible, claimed)
			}
		})
	}
}
