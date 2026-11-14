package storage

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// Bump this key whenever the batch checks get stricter to force a full rescan.
const legacyReviewScanKey = "legacy_review_scan_v1"

type legacyReviewScanState struct {
	Done *time.Time
	Run  *legacyReviewScanRun
}

type legacyReviewScanRun struct {
	Start time.Time
	After *uuid.UUID
}

func (p *PgPool) migrateLegacyReviews(ctx context.Context, noWait bool) (err error) {
	total := 0
	defer func() {
		if err != nil && total > 0 {
			p.legacyProgress.Add(int64(total))
		}
	}()
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS legacy_reviews (
 uuid UUID PRIMARY KEY, record JSONB NOT NULL, migration_error TEXT NOT NULL, resolved_at TIMESTAMPTZ)`); err != nil {
		return err
	}
	var state legacyReviewScanState
	var stored string
	err = p.pool.QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = $1`, legacyReviewScanKey).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read historical mirror scan state: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal([]byte(stored), &state); err != nil {
			log.Printf("Historical mirror review migration: unreadable scan state; restarting full scan: %v", err)
			state = legacyReviewScanState{}
		}
	}
	if state.Run == nil {
		state.Run = &legacyReviewScanRun{}
		if err := p.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&state.Run.Start); err != nil {
			return fmt.Errorf("start historical mirror scan: %w", err)
		}
	}
	for {
		next, scanned, err := p.migrateLegacyReviewsBatch(ctx, state.Run.After, state.Done, noWait)
		if err != nil {
			return fmt.Errorf("archive historical mirror reviews: %w", err)
		}
		if scanned == 0 {
			state.Done = new(state.Run.Start.Add(-defaultSyncCursorLookback))
			state.Run = nil
		} else {
			state.Run.After = next
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("encode historical mirror scan state: %w", err)
		}
		if _, err := p.pool.Exec(ctx, `INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, legacyReviewScanKey, string(raw)); err != nil {
			return fmt.Errorf("save historical mirror scan state: %w", err)
		}
		if scanned == 0 {
			return nil
		}
		total += scanned
		log.Printf("Historical mirror review migration: scanned %d reviews", total)
	}
}

func (p *PgPool) migrateLegacyReviewsBatch(ctx context.Context, after *uuid.UUID, since *time.Time, noWait bool) (*uuid.UUID, int, error) {
	scanned := 0
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return after, scanned, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockClause := " FOR UPDATE OF r"
	if noWait {
		lockClause += " NOWAIT"
	}
	rows, err := tx.Query(ctx, `SELECT r.uuid, r.output, r.structured_output, j.job_type, j.uuid,
 COALESCE(j.min_severity, ''), r.verdict_bool
 FROM reviews r JOIN review_jobs j ON j.uuid = r.job_uuid
 WHERE j.job_type IN ('review','range','dirty','synthesis','compact')
 AND ($1::uuid IS NULL OR r.uuid > $1)
 AND ($3::timestamptz IS NULL OR r.updated_at >= $3)
 ORDER BY r.uuid LIMIT $2`+lockClause, after, legacyReviewBatchSize, since)
	if err != nil {
		return after, scanned, err
	}
	type update struct {
		id     uuid.UUID
		raw    jsontext.Value
		reason string
		// markdown is set when the row has no usable JSON. Conversion waits
		// until the row cursor is closed because it may query panel members.
		markdown *legacyMarkdown
		jobUUID  uuid.UUID
		verdict  *bool
	}
	var updates []update
	for rows.Next() {
		var u update
		var output, jobType, threshold string
		var storedVerdict *bool
		if err := rows.Scan(&u.id, &output, &u.raw, &jobType, &u.jobUUID, &threshold, &storedVerdict); err != nil {
			rows.Close()
			return after, scanned, err
		}
		after = new(u.id)
		scanned++
		if len(u.raw) == 0 {
			u.raw = jsontext.Value(output)
		}
		doc, decodeErr := structuredreview.Decode(u.raw)
		if decodeErr == nil && jobType == JobTypeSynthesis {
			decodeErr = doc.RequireSources(len(doc.SourceLabels))
		}
		if decodeErr != nil {
			u.reason = "No valid review JSON document; AI conversion required: " + decodeErr.Error()
			u.markdown = &legacyMarkdown{Markdown: output, JobType: jobType, MinSeverity: threshold, StoredVerdict: storedVerdict}
		} else if output == "" {
			continue
		}
		updates = append(updates, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return after, scanned, err
	}
	for i := range updates {
		u := &updates[i]
		if u.markdown == nil {
			continue
		}
		if u.markdown.JobType == JobTypeSynthesis {
			sources, err := postgresLegacySources(ctx, tx, u.jobUUID)
			if err != nil {
				return after, scanned, err
			}
			u.markdown.SourceLabels = legacySourceLabels(sources)
		}
		raw, refusal := convertLegacyMarkdown(*u.markdown)
		if refusal != nil {
			u.reason += "; automatic conversion refused (" + refusal.Reason + "): " + refusal.Detail
			continue
		}
		u.raw, u.reason = raw, ""
		if doc, err := structuredreview.Decode(raw); err == nil && !doc.UnableToReview() {
			u.verdict = new(doc.Passed(u.markdown.MinSeverity))
		}
	}
	for _, u := range updates {
		if _, err := tx.Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error, resolved_at)
   SELECT r.uuid, to_jsonb(r), $2, CASE WHEN $2 = '' THEN clock_timestamp() ELSE NULL END
   FROM reviews r WHERE r.uuid = $1 ON CONFLICT(uuid) DO NOTHING`, u.id, u.reason); err != nil {
			return after, scanned, err
		}
		if u.reason != "" {
			if _, err := tx.Exec(ctx, `DELETE FROM reviews WHERE uuid = $1`, u.id); err != nil {
				return after, scanned, err
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE reviews SET output = '', structured_output = $2, updated_at = clock_timestamp(),
    verdict_bool = CASE WHEN $3 THEN $4::boolean ELSE verdict_bool END
    WHERE uuid = $1`, u.id, []byte(u.raw), u.markdown != nil, u.verdict); err != nil {
				return after, scanned, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return after, scanned, err
	}
	p.legacyProgress.Add(int64(len(updates)))
	return after, scanned, nil
}
