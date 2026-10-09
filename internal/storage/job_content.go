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

var contentCodec = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
})

var contentDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil)
})

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
	enc, err := contentCodec()
	if err != nil {
		return nil, fmt.Errorf("zstd_compress: %w", err)
	}
	return enc.EncodeAll(text, nil), nil
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
	dec, err := contentDecoder()
	if err != nil {
		return nil, fmt.Errorf("zstd_decompress: %w", err)
	}
	text, err := dec.DecodeAll(compressed, nil)
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
// kept. It returns the number of prompts removed.
func (db *DB) PruneJobPrompts(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := db.ExecContext(ctx, `
		UPDATE job_content SET prompt = NULL
		WHERE prompt IS NOT NULL AND job_id IN (
			SELECT id FROM review_jobs
			WHERE job_type IN ('review', 'range')
			  AND status NOT IN ('queued', 'running')
			  AND finished_at IS NOT NULL
			  AND julianday(finished_at) < julianday(?)
		)`, cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("prune job prompts: %w", err)
	}
	return result.RowsAffected()
}

// jobContentMoveBatchSize bounds how many jobs one migration transaction
// moves, so an interrupted upgrade keeps the work already committed.
const jobContentMoveBatchSize = 500

// migrateJobContent moves the payloads of databases created before
// job_content existed. Those databases store the prompt in review_jobs, again
// in reviews, and a third time in legacy_reviews. The job's prompt is kept;
// a job without one takes its review's prompt so no prompt's only copy is
// lost. After the copy, the old columns are dropped and the database is
// vacuumed once to return the freed space to the filesystem.
func (db *DB) migrateJobContent() error {
	ctx := context.Background()
	legacy, err := hasColumn(ctx, db, "reviews", "prompt")
	if err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	start := time.Now()
	log.Printf("Database migration: compressing job prompts, diffs, and patches")
	moved, err := db.copyLegacyJobContent(ctx)
	if err != nil {
		return err
	}
	if err := db.dropLegacyContentColumns(ctx); err != nil {
		return err
	}
	log.Printf("Database migration: compressed content of %d jobs in %s; reclaiming disk space",
		moved, time.Since(start).Round(time.Second))
	vacuumStart := time.Now()
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		// Freed pages stay in the file and are reused by later writes.
		log.Printf("Database migration: VACUUM failed, free space stays inside the database file: %v", err)
		return nil
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("Database migration: WAL checkpoint after VACUUM failed: %v", err)
	}
	log.Printf("Database migration: VACUUM finished in %s", time.Since(vacuumStart).Round(time.Second))
	return nil
}

func (db *DB) copyLegacyJobContent(ctx context.Context) (int, error) {
	exprs := map[string]string{"prompt": "NULL", "diff_content": "NULL", "patch": "NULL"}
	for column := range exprs {
		present, err := hasColumn(ctx, db, "review_jobs", column)
		if err != nil {
			return 0, err
		}
		if present {
			exprs[column] = "j." + column
		}
	}
	// A job saved its prompt only once it started running, so jobs from
	// before that change keep their prompt on the review alone.
	promptExpr := fmt.Sprintf(
		`COALESCE(NULLIF(%s, ''), (SELECT rv.prompt FROM reviews rv WHERE rv.job_id = j.id))`,
		exprs["prompt"])
	copySQL := fmt.Sprintf(`
		INSERT INTO job_content (job_id, prompt, diff_content, patch)
		SELECT j.id, zstd_compress(%[1]s), zstd_compress(%[2]s), zstd_compress(%[3]s)
		FROM review_jobs j
		WHERE j.id > ? AND j.id <= ?
		  AND COALESCE(NULLIF(%[1]s, ''), NULLIF(%[2]s, ''), NULLIF(%[3]s, '')) IS NOT NULL
		ON CONFLICT(job_id) DO UPDATE SET
		  prompt = excluded.prompt, diff_content = excluded.diff_content, patch = excluded.patch`,
		promptExpr, exprs["diff_content"], exprs["patch"])

	var maxID int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM review_jobs`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("find last job: %w", err)
	}
	moved := 0
	lastLog := time.Now()
	for after := int64(0); after < maxID; after += jobContentMoveBatchSize {
		result, err := db.ExecContext(ctx, copySQL, after, after+jobContentMoveBatchSize)
		if err != nil {
			return moved, fmt.Errorf("compress content of jobs %d-%d: %w",
				after+1, after+jobContentMoveBatchSize, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return moved, fmt.Errorf("count compressed jobs: %w", err)
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

func (db *DB) dropLegacyContentColumns(ctx context.Context) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dropping content columns: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
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
