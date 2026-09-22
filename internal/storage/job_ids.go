package storage

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// migrateJobIDs makes SQLite job IDs permanent identities, including IDs whose
// only remaining reference is an archived review. Existing IDs stay unchanged.
func (db *DB) migrateJobIDs() error {
	var original string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'review_jobs'`).Scan(&original); err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(original), "AUTOINCREMENT") {
		return nil
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Save every index and trigger before rebuilding the table.
	rows, err := tx.Query(`SELECT sql FROM sqlite_master WHERE tbl_name = 'review_jobs' AND type IN ('index', 'trigger') AND sql IS NOT NULL`)
	if err != nil {
		return err
	}
	var definitions []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			rows.Close()
			return err
		}
		definitions = append(definitions, definition)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	start := strings.Index(original, "(")
	if start < 0 || !strings.Contains(original, "id INTEGER PRIMARY KEY") {
		return fmt.Errorf("unrecognized review_jobs primary key")
	}
	create := "CREATE TABLE review_jobs_new " + strings.Replace(original[start:], "id INTEGER PRIMARY KEY", "id INTEGER PRIMARY KEY AUTOINCREMENT", 1)
	if _, err := tx.Exec(create); err != nil {
		return err
	}
	// Keep the schema replacement atomic, but copy bounded sets of rows per
	// statement to bound the work performed by each copy statement.
	var after int64
	copied := int64(0)
	for {
		result, err := tx.Exec(`INSERT INTO review_jobs_new SELECT * FROM review_jobs WHERE id > ? ORDER BY id LIMIT ?`, after, legacyReviewBatchSize)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			break
		}
		if err := tx.QueryRow(`SELECT max(id) FROM review_jobs_new`).Scan(&after); err != nil {
			return err
		}
		copied += count
		log.Printf("Job ID migration: copied %d jobs (through job %d)", copied, after)
	}
	if _, err := tx.Exec(`DROP TABLE review_jobs`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE review_jobs_new RENAME TO review_jobs`); err != nil {
		return err
	}
	for i, definition := range definitions {
		log.Printf("Job ID migration: recreating index/trigger %d of %d", i+1, len(definitions))
		if _, err := tx.Exec(definition); err != nil {
			return err
		}
	}
	// An archive may outlive a deleted job from a previous release. Do not let
	// a new job take its ID, even when it exceeds all surviving job IDs.
	if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq) SELECT 'review_jobs', 0 WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'review_jobs')`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sqlite_sequence SET seq = max(seq, COALESCE((SELECT max(job_id) FROM legacy_reviews), 0)) WHERE name = 'review_jobs'`); err != nil {
		return err
	}
	return tx.Commit()
}
