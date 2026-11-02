package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkCIPollPanelHistory measures one poll's panel lookups across open PRs
// while retired, posted history accumulates on other PRs in the same repo.
func BenchmarkCIPollPanelHistory(b *testing.B) {
	for _, history := range []int{1000, 10000, 100000} {
		for _, openPRs := range []int{50, 200} {
			b.Run(fmt.Sprintf("history-%d/open-%d", history, openPRs), func(b *testing.B) {
				db, err := Open(filepath.Join(b.TempDir(), "reviews.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, db.Close()) })
				_, err = db.ExecContext(b.Context(), `
					WITH RECURSIVE sequence(n) AS (
						SELECT 1 UNION ALL SELECT n + 1 FROM sequence WHERE n < ?
					)
					INSERT INTO ci_pr_panels
						(github_repo, pr_number, head_sha, panel_run_uuid, retired_at, posted_at, outcome)
					SELECT 'acme/api', n + ?, 'old-head',
						printf('00000000-0000-4000-8000-%012d', n),
						'2026-01-01 00:00:00', '2026-01-01 00:00:00', ?
					FROM sequence`, history, openPRs, PanelOutcomeReviewPosted)
				require.NoError(b, err)

				b.ReportAllocs()
				for b.Loop() {
					for pr := 1; pr <= openPRs; pr++ {
						panels, err := db.GetUnpostedPanelsForPR("acme/api", pr)
						require.NoError(b, err)
						require.Empty(b, panels)
						_, err = db.GetActiveCIPanelByPRSHA("acme/api", pr, "head")
						require.ErrorIs(b, err, sql.ErrNoRows)
						posted, err := db.HasPostedCIReview("acme/api", pr, "head")
						require.NoError(b, err)
						require.False(b, posted)
					}
				}
			})
		}
	}
}
