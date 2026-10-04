package storage

import (
	"context"
	"fmt"
	"strings"
)

// migrateRepoIDs prevents deleted repository IDs from being reassigned, which
// would transfer remote access grants to a different repository. Existing IDs
// and their references stay unchanged.
func (db *DB) migrateRepoIDs() error {
	var original string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'repos'`).Scan(&original); err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(original), "AUTOINCREMENT") {
		return nil
	}
	start := strings.Index(original, "(")
	if start < 0 || !strings.Contains(original, "id INTEGER PRIMARY KEY") {
		return fmt.Errorf("unrecognized repos primary key")
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

	rows, err := tx.Query(`SELECT sql FROM sqlite_master WHERE tbl_name = 'repos' AND type IN ('index', 'trigger') AND sql IS NOT NULL`)
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

	create := "CREATE TABLE repos_new " + strings.Replace(original[start:], "id INTEGER PRIMARY KEY", "id INTEGER PRIMARY KEY AUTOINCREMENT", 1)
	if _, err := tx.Exec(create); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO repos_new SELECT * FROM repos`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE repos`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE repos_new RENAME TO repos`); err != nil {
		return err
	}
	for _, definition := range definitions {
		if _, err := tx.Exec(definition); err != nil {
			return err
		}
	}
	return tx.Commit()
}
