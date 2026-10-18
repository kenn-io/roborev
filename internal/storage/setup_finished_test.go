package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"testing"
	"time"
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

type cancelSetupClaimConnector struct {
	driver driver.Driver
	dsn    string
	cancel context.CancelFunc
}

func (c cancelSetupClaimConnector) Driver() driver.Driver { return c.driver }

func (c cancelSetupClaimConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &cancelSetupClaimConn{Conn: conn, cancel: c.cancel}, nil
}

type cancelSetupClaimConn struct {
	driver.Conn
	cancel context.CancelFunc
}

func (c *cancelSetupClaimConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && len(args) == 1 && args[0].Value == SyncStateSetupFinished {
		// Cancel after SQLite commits, before the driver reports its result.
		c.cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return result, err
}

func TestClaimSetupFinishedKeepsCommittedResultAfterCancellation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repoID, commitID := seedSearchFeedBase(t, db)
	seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "Review"})
	var path string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	pool := sql.OpenDB(cancelSetupClaimConnector{driver: db.Driver(), dsn: path + "?_pragma=busy_timeout(30000)", cancel: cancel})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	claimDB := &DB{DB: pool}
	claim, err := claimDB.ClaimSetupFinished(ctx)
	require.NoError(t, err)
	assert.Equal(t, SetupFinishedNewClaim, claim)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	marker, err := db.GetSyncState(SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Equal(t, "1", marker)
}
