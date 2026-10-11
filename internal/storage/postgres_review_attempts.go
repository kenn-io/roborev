package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// migrateReviewAttempts associates the current historical review with its
// parent's attempt once, after legacy restoration. Older history remains
// unassociated: a later restart must not relabel it as a newer attempt.
func (p *PgPool) migrateReviewAttempts(ctx context.Context) error {
	return p.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE schema_version IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
			return err
		}
		if version >= 24 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE reviews r SET job_enqueued_at = j.enqueued_at
			FROM review_jobs j WHERE r.job_uuid = j.uuid AND r.job_enqueued_at IS NULL
			AND j.status IN ('done', 'applied', 'rebased')
			AND NOT EXISTS (SELECT 1 FROM reviews newer WHERE newer.job_uuid = r.job_uuid
				AND (newer.created_at, newer.id) > (r.created_at, r.id))`); err != nil {
			return fmt.Errorf("v24 migration (backfill review attempt): %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE legacy_reviews l
			SET record = l.record || jsonb_build_object('job_enqueued_at', r.job_enqueued_at)
			FROM reviews r WHERE l.uuid = r.uuid AND r.job_enqueued_at IS NOT NULL`); err != nil {
			return fmt.Errorf("v24 migration (archive review attempt): %w", err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO schema_version (version) VALUES (24) ON CONFLICT DO NOTHING`)
		return err
	})
}
