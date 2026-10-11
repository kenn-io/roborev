package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"modernc.org/sqlite"
)

// Large job payloads (prompt, frozen dirty diff, fix patch) live in
// job_content, one row per job, compressed with zstd. Keeping them out of
// review_jobs keeps that table's rows small, so listing and counting jobs reads
// metadata pages only. A NULL column means the job has no such payload.
//
// The zstd_compress and zstd_decompress SQL functions do the encoding, so
// queries read and write plain strings: zstd_compress(?) on write and
// zstd_decompress(jc.prompt) on read. Both map NULL and '' to NULL.

// The codecs are built when the package loads. Each hands out its workers
// through a Go channel, and a channel first used inside a testing/synctest
// bubble cannot be used outside it, which crashes the test binary. Building
// them here keeps those channels outside every bubble.
var (
	contentEncoder, contentEncoderErr = newContentEncoder()
	contentDecoder, contentDecoderErr = zstd.NewReader(nil)
)

func newContentEncoder() (*zstd.Encoder, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	// The encoder creates its worker channel on its first call.
	enc.EncodeAll(nil, nil)
	return enc, nil
}

// registerContentFunctions makes the zstd SQL functions available to every
// connection the sqlite driver opens afterwards. VolatileArgs makes the driver
// pass TEXT arguments with their full length; without it, text is copied as a
// C string and a prompt is cut at its first NUL byte. Neither function keeps
// its argument after returning, which the flag requires.
var registerContentFunctions = sync.OnceValue(func() error {
	for name, scalar := range map[string]func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error){
		"zstd_compress":   sqlZstdCompress,
		"zstd_decompress": sqlZstdDecompress,
	} {
		impl := &sqlite.FunctionImpl{NArgs: 1, Deterministic: true, VolatileArgs: true, Scalar: scalar}
		if err := sqlite.RegisterFunction(name, impl); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
	}
	return nil
})

func sqlZstdCompress(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	var text []byte
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case string:
		text = []byte(v)
	case []byte:
		text = v
	default:
		return nil, fmt.Errorf("zstd_compress: unsupported argument type %T", v)
	}
	if len(text) == 0 {
		return nil, nil
	}
	if contentEncoderErr != nil {
		return nil, fmt.Errorf("zstd_compress: %w", contentEncoderErr)
	}
	return contentEncoder.EncodeAll(text, nil), nil
}

func sqlZstdDecompress(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	var compressed []byte
	switch v := args[0].(type) {
	case nil:
		return nil, nil
	case []byte:
		compressed = v
	default:
		return nil, fmt.Errorf("zstd_decompress: expected BLOB, got %T", v)
	}
	if len(compressed) == 0 {
		return nil, nil
	}
	if contentDecoderErr != nil {
		return nil, fmt.Errorf("zstd_decompress: %w", contentDecoderErr)
	}
	text, err := contentDecoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd_decompress: %w", err)
	}
	return string(text), nil
}

// jobContentColumn names one payload column of job_content.
type jobContentColumn string

const (
	jobContentPrompt jobContentColumn = "prompt"
	jobContentDiff   jobContentColumn = "diff_content"
	jobContentPatch  jobContentColumn = "patch"
)

// setJobContent stores one payload for a job, compressing it. An empty value
// clears the payload.
func setJobContent(ctx context.Context, exec execer, jobID int64, column jobContentColumn, value string) error {
	query := fmt.Sprintf(`INSERT INTO job_content (job_id, %[1]s) VALUES (?, zstd_compress(?))
		ON CONFLICT(job_id) DO UPDATE SET %[1]s = excluded.%[1]s`, column)
	if _, err := exec.ExecContext(ctx, query, jobID, value); err != nil {
		return fmt.Errorf("store job %d %s: %w", jobID, column, err)
	}
	return nil
}

// clearJobContent removes one payload from a job. Jobs without a payload row
// are left alone.
func clearJobContent(ctx context.Context, exec execer, jobID int64, column jobContentColumn) error {
	query := fmt.Sprintf(`UPDATE job_content SET %s = NULL WHERE job_id = ?`, column)
	if _, err := exec.ExecContext(ctx, query, jobID); err != nil {
		return fmt.Errorf("clear job %d %s: %w", jobID, column, err)
	}
	return nil
}

// PruneJobPrompts removes the stored prompts of review and range jobs that
// finished before cutoff. Their prompts are rebuilt from git if the job is
// rerun, so only the prompt view of an old job loses content. Reviews,
// findings, diffs of dirty reviews, patches, and prompts of jobs whose prompt
// cannot be rebuilt (tasks, fixes, compaction, insights, goal reviews) are
// kept. With keepUnpushed, a job that sync has yet to push keeps its prompt,
// so retention cannot remove a prompt before PostgreSQL has a copy. It returns
// the number of prompts removed.
func (db *DB) PruneJobPrompts(ctx context.Context, cutoff time.Time, keepUnpushed bool) (int64, error) {
	pushedFilter := ""
	if keepUnpushed {
		pushedFilter = `AND synced_at IS NOT NULL
			  AND ` + sqliteNormalizedTimestampExpr("updated_at") + ` <= ` + sqliteNormalizedTimestampExpr("synced_at")
	}
	result, err := db.ExecContext(ctx, `
		UPDATE job_content SET prompt = NULL
		WHERE prompt IS NOT NULL AND job_id IN (
			SELECT id FROM review_jobs
			WHERE job_type IN ('review', 'range')
			  AND status NOT IN ('queued', 'running')
			  AND finished_at IS NOT NULL
			  AND julianday(finished_at) < julianday(?)
			  `+pushedFilter+`
		)`, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("prune job prompts: %w", err)
	}
	return result.RowsAffected()
}

// jobContentMoveBatchSize bounds how many jobs one migration transaction
// moves, so an interrupted upgrade keeps the work already committed.
const jobContentMoveBatchSize = 500

// migrateJobContent is schema migration 2. Before it, a database stores each
// prompt in review_jobs, again in reviews, and a third time in legacy_reviews,
// inline with the metadata of those rows. It moves each job's prompt, diff,
// and patch into job_content, compressed, then drops the old columns and
// vacuums once to return the freed space to the filesystem. The job's prompt
// wins, except over the review of a job pulled from another machine; a
// completed job without one takes its review's or archived review's prompt,
// so no prompt's only copy is lost.
//
// A rerun is safe: the copy redoes each batch from the old columns, which
// stay until it finishes, and the columns are dropped in one transaction,
// after which a rerun only repeats the VACUUM.
func migrateJobContent(ctx context.Context, db *DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS job_content (
		job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id),
		prompt BLOB,
		diff_content BLOB,
		patch BLOB
	)`); err != nil {
		return fmt.Errorf("create job_content: %w", err)
	}
	pending, err := hasColumn(ctx, db, "reviews", "prompt")
	if err != nil {
		return err
	}
	if pending {
		start := time.Now()
		moved, err := db.copyLegacyJobContent(ctx)
		if err != nil {
			return err
		}
		if err := db.dropLegacyContentColumns(ctx); err != nil {
			return err
		}
		log.Printf("Database migration: compressed content of %d jobs in %s; reclaiming disk space",
			moved, time.Since(start).Round(time.Second))
	}
	vacuumStart := time.Now()
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		// Freed pages stay in the file and are reused by later writes.
		log.Printf("Database migration: VACUUM failed, free space stays inside the database file: %v", err)
	} else {
		log.Printf("Database migration: VACUUM finished in %s", time.Since(vacuumStart).Round(time.Second))
	}
	// Shrink the WAL, which grew by the size of the column drops, either way.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("Database migration: WAL checkpoint failed: %v", err)
	}
	return nil
}

func (db *DB) copyLegacyJobContent(ctx context.Context) (int, error) {
	// Jobs from before review_jobs.prompt existed keep their prompt only on
	// the review, or only in the archive when the review was archived.
	archivedPrompt, err := hasColumn(ctx, db, "legacy_reviews", "prompt")
	if err != nil {
		return 0, err
	}
	archiveExpr := "NULL"
	if archivedPrompt {
		// The archive's own job index covers only unresolved rows. Without a
		// full one, each lookup scans every archived prompt.
		if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_legacy_reviews_job_content_move
			ON legacy_reviews(job_id)`); err != nil {
			return 0, fmt.Errorf("index archived reviews by job: %w", err)
		}
		archiveExpr = `(SELECT l.prompt FROM legacy_reviews l
			WHERE l.job_id = j.id AND l.prompt != '' ORDER BY l.archive_id DESC LIMIT 1)`
	}
	promptExpr := legacyJobPromptExpr(archiveExpr)
	copySQL := fmt.Sprintf(`
		INSERT INTO job_content (job_id, prompt, diff_content, patch)
		SELECT j.id, zstd_compress(%[1]s), zstd_compress(j.diff_content), zstd_compress(j.patch)
		FROM review_jobs j
		WHERE j.id > ? AND j.id <= ?
		  AND COALESCE(NULLIF(%[1]s, ''), NULLIF(j.diff_content, ''), NULLIF(j.patch, '')) IS NOT NULL`,
		promptExpr)
	// PostgreSQL may lack a prompt that differs from the job's: an older
	// client pushed the job's prompt and could stop before pushing the
	// review that held the complete one, and reviews no longer carry a prompt.
	// Clearing synced_at makes the next sync push the job with that prompt.
	// Only this machine's jobs are pushed; a pulled job keeps its marker,
	// which the sync of its reviews and comments requires.
	resyncSQL := `UPDATE review_jobs AS j SET synced_at = NULL
		WHERE j.id > ? AND j.id <= ? AND j.synced_at IS NOT NULL
		  AND lower(j.source_machine_id) = (SELECT lower(value) FROM sync_state WHERE key = 'machine_id')
		  AND ` + promptExpr + ` IS NOT NULLIF(j.prompt, '')`

	var maxID int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM review_jobs`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("find last job: %w", err)
	}
	moved := 0
	lastLog := time.Now()
	for after := int64(0); after < maxID; after += jobContentMoveBatchSize {
		count, err := db.copyJobContentBatch(ctx, copySQL, resyncSQL, after, after+jobContentMoveBatchSize)
		if err != nil {
			return moved, fmt.Errorf("compress content of jobs %d-%d: %w",
				after+1, after+jobContentMoveBatchSize, err)
		}
		moved += int(count)
		if time.Since(lastLog) >= 10*time.Second {
			log.Printf("Database migration: compressed content through job %d of %d",
				min(after+jobContentMoveBatchSize, maxID), maxID)
			lastLog = time.Now()
		}
	}
	return moved, nil
}

// legacyJobPromptExpr selects the prompt a job j keeps when its old copies
// move into job_content. archiveExpr selects the job's newest archived review
// prompt, or is NULL.
func legacyJobPromptExpr(archiveExpr string) string {
	// A CI panel member's job kept its prebuilt prompt, with the diff
	// placeholders a retry needs, but without the member's reviewer
	// instructions. The worker appended those to the prompt it sent and to the
	// review's copy only. Restore them, in the format memberInstructionSuffix
	// in internal/daemon/worker.go used when this migration was written.
	memberInstructions := `json_extract(j.panel_member_config_json, '$.instructions')`
	memberSuffix := `CASE WHEN j.prompt_prebuilt != 0 AND j.panel_role = 'member'
		AND json_valid(j.panel_member_config_json)
		AND json_type(j.panel_member_config_json, '$.instructions') = 'text'
		AND ` + memberInstructions + ` != ''
		THEN char(10, 10) || '## Additional reviewer instructions (panel: ' || COALESCE(j.panel_name, '')
			|| ' / member: ' || COALESCE(j.panel_member_name, '') || ')' || char(10)
			|| ` + memberInstructions + ` || char(10)
		ELSE '' END`
	reviewPrompt := `(SELECT NULLIF(rv.prompt, '') FROM reviews rv WHERE rv.job_id = j.id)`
	// A job pulled from another machine kept the prompt of the attempt it was
	// first pulled at, because pulls never updated it, while its pulled
	// review, or the archive of that review when no review remains, belongs to
	// the latest completed attempt. Prefer those for jobs whose prompt a rerun rebuilds; the others
	// keep their prompt across reruns, and their review's copy carries an
	// agent preamble. A copy that only points to a prompt file is skipped in
	// favor of the job's complete prompt.
	remoteReviewPrompt := `CASE WHEN j.status = 'done'
		AND COALESCE(j.job_type, 'review') NOT IN ` + storedPromptJobTypesSQL + `
		AND j.source_machine_id IS NOT NULL
		AND lower(j.source_machine_id) != (SELECT lower(value) FROM sync_state WHERE key = 'machine_id')
		THEN CASE WHEN EXISTS (SELECT 1 FROM reviews rv WHERE rv.job_id = j.id)
			THEN ` + withoutPromptFileHandoff(reviewPrompt) + `
			ELSE ` + withoutPromptFileHandoff(archiveExpr) + ` END END`
	// Only a completed attempt's review or archived review holds the job's
	// prompt. A rerun deletes the review but not its archived copy, so a rerun
	// that stopped before saving its prompt must not take the earlier one.
	return `COALESCE(` + remoteReviewPrompt + `,
		NULLIF(j.prompt, '') || ` + memberSuffix + `,
		CASE WHEN j.status IN ('done', 'applied', 'rebased') THEN COALESCE(` + reviewPrompt + `, ` + archiveExpr + `) END)`
}

// storedPromptJobTypesSQL lists, as an SQL list, the job types whose stored
// prompt is the input a rerun reuses rather than rebuilds.
const storedPromptJobTypesSQL = `('task', 'compact', 'fix', 'insights', 'goal_review')`

// promptFileHandoffPattern is a LIKE pattern for the one-line prompt that
// prompt.Builder.Prepare sends in place of an oversized prompt since v0.68.0.
// Releases before job_content stored that line as the review's prompt copy,
// keeping the complete prompt only on the job. The file it names is deleted
// after the attempt, so the line is never a usable prompt.
const promptFileHandoffPattern = `Read the complete task prompt from "%" and carry out its instructions. ` +
	`Read the file in full before starting.%`

// withoutPromptFileHandoff returns SQL that yields expr, or NULL when expr is
// a prompt file handoff line. It works in SQLite and PostgreSQL.
func withoutPromptFileHandoff(expr string) string {
	return `CASE WHEN ` + expr + ` LIKE '` + promptFileHandoffPattern + `' THEN NULL ELSE ` + expr + ` END`
}

// copyJobContentBatch replaces the content of jobs with IDs in (after, through]
// and marks the jobs that need a new push in the same transaction, so an
// interrupted copy never leaves a moved prompt unsynced. The batch first
// removes rows an interrupted earlier run left, because an older release
// may have changed or cleared the job's payload since.
func (db *DB) copyJobContentBatch(ctx context.Context, copySQL, resyncSQL string, after, through int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM job_content WHERE job_id > ? AND job_id <= ?`,
		after, through); err != nil {
		return 0, fmt.Errorf("remove partly copied content: %w", err)
	}
	result, err := tx.ExecContext(ctx, copySQL, after, through)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count compressed jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, resyncSQL, after, through); err != nil {
		return 0, fmt.Errorf("mark jobs for sync: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (db *DB) dropLegacyContentColumns(ctx context.Context) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dropping content columns: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_legacy_reviews_job_content_move`); err != nil {
		return fmt.Errorf("drop temporary archive index: %w", err)
	}
	for _, target := range []struct{ table, column string }{
		{"review_jobs", "prompt"},
		{"review_jobs", "diff_content"},
		{"review_jobs", "patch"},
		{"reviews", "prompt"},
		{"legacy_reviews", "prompt"},
	} {
		present, err := hasColumn(ctx, tx, target.table, target.column)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`,
			target.table, target.column)); err != nil {
			return fmt.Errorf("drop %s.%s: %w", target.table, target.column, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dropping content columns: %w", err)
	}
	return nil
}

type sqlQueryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func hasColumn(ctx context.Context, q sqlQueryRower, table, column string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check %s.%s column: %w", table, column, err)
	}
	return count > 0, nil
}
