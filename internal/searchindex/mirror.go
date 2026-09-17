package searchindex

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/roborev/internal/searchdoc"
)

type mirrorRow struct {
	DocKey      string
	ReviewID    int64
	ReviewUUID  string
	JobID       int64
	JobUUID     string
	GroupKey    string
	RepoID      int64
	RepoName    string
	Branch      string
	GitRef      string
	CommitSHA   string
	FinishedAt  string
	Verdict     string
	Closed      int
	PanelRole   string
	Content     string
	ContentHash string
	Identifiers string
}

// RefreshMirrorPage writes changed documents and their lexical rows together.
func (index *Index) RefreshMirrorPage(ctx context.Context, docs []searchdoc.Document, seen map[string]struct{}) (changed int, err error) {
	tx, err := index.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin search mirror refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	keys := make([]string, 0, len(docs))
	for _, doc := range docs {
		keys = append(keys, doc.DocKey)
		wanted := rowForDocument(doc)
		current, found, err := readMirrorRow(ctx, tx, doc.DocKey)
		if err != nil {
			return 0, err
		}
		if found && current == wanted {
			continue
		}
		if err := upsertMirrorRow(ctx, tx, wanted); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_fts WHERE doc_key = ?`, doc.DocKey); err != nil {
			return 0, fmt.Errorf("delete stale review FTS row: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_fts(doc_key, content, identifiers) VALUES (?, ?, ?)`,
			doc.DocKey, doc.Content, doc.Identifiers); err != nil {
			return 0, fmt.Errorf("insert review FTS row: %w", err)
		}
		changed++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit search mirror refresh: %w", err)
	}
	if seen != nil {
		for _, key := range keys {
			seen[key] = struct{}{}
		}
	}
	return changed, nil
}

func rowForDocument(doc searchdoc.Document) mirrorRow {
	finishedAt := ""
	if !doc.Source.FinishedAt.IsZero() {
		finishedAt = doc.Source.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	closed := 0
	if doc.Source.Closed {
		closed = 1
	}
	return mirrorRow{
		DocKey: doc.DocKey, ReviewID: doc.Source.ReviewID, ReviewUUID: doc.Source.ReviewUUID,
		JobID: doc.Source.JobID, JobUUID: doc.Source.JobUUID, GroupKey: doc.GroupKey,
		RepoID: doc.Source.RepoID, RepoName: doc.Source.RepoName, Branch: doc.Source.Branch,
		GitRef: doc.Source.GitRef, CommitSHA: doc.Source.CommitSHA, FinishedAt: finishedAt,
		Verdict: doc.Source.Verdict, Closed: closed, PanelRole: doc.Source.PanelRole,
		Content: doc.Content, ContentHash: doc.ContentHash, Identifiers: doc.Identifiers,
	}
}

func readMirrorRow(ctx context.Context, tx *sql.Tx, docKey string) (mirrorRow, bool, error) {
	var row mirrorRow
	err := tx.QueryRowContext(ctx, `
		SELECT m.doc_key, m.review_id, COALESCE(m.review_uuid, ''),
		       m.job_id, COALESCE(m.job_uuid, ''), m.group_key,
		       m.repo_id, m.repo_name, COALESCE(m.branch, ''), m.git_ref,
		       COALESCE(m.commit_sha, ''), COALESCE(m.finished_at, ''),
		       COALESCE(m.verdict, ''), m.closed, COALESCE(m.panel_role, ''),
		       m.content, m.content_hash,
		       COALESCE((SELECT identifiers FROM review_fts f WHERE f.doc_key = m.doc_key LIMIT 1), '')
		  FROM review_mirror m WHERE m.doc_key = ?`, docKey).Scan(
		&row.DocKey, &row.ReviewID, &row.ReviewUUID, &row.JobID, &row.JobUUID,
		&row.GroupKey, &row.RepoID, &row.RepoName, &row.Branch, &row.GitRef,
		&row.CommitSHA, &row.FinishedAt, &row.Verdict, &row.Closed, &row.PanelRole,
		&row.Content, &row.ContentHash, &row.Identifiers)
	if err == sql.ErrNoRows {
		return mirrorRow{}, false, nil
	}
	if err != nil {
		return mirrorRow{}, false, fmt.Errorf("read search mirror row: %w", err)
	}
	return row, true, nil
}

func upsertMirrorRow(ctx context.Context, tx *sql.Tx, row mirrorRow) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO review_mirror (
			doc_key, review_id, review_uuid, job_id, job_uuid, group_key,
			repo_id, repo_name, branch, git_ref, commit_sha, finished_at,
			verdict, closed, panel_role, content, content_hash, embed_gen
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(doc_key) DO UPDATE SET
			review_id = excluded.review_id,
			review_uuid = excluded.review_uuid,
			job_id = excluded.job_id,
			job_uuid = excluded.job_uuid,
			group_key = excluded.group_key,
			repo_id = excluded.repo_id,
			repo_name = excluded.repo_name,
			branch = excluded.branch,
			git_ref = excluded.git_ref,
			commit_sha = excluded.commit_sha,
			finished_at = excluded.finished_at,
			verdict = excluded.verdict,
			closed = excluded.closed,
			panel_role = excluded.panel_role,
			content = excluded.content,
			content_hash = excluded.content_hash,
			embed_gen = CASE
				WHEN review_mirror.content_hash IS excluded.content_hash THEN review_mirror.embed_gen
				ELSE NULL
			END`,
		row.DocKey, row.ReviewID, nullable(row.ReviewUUID), row.JobID, nullable(row.JobUUID), row.GroupKey,
		row.RepoID, row.RepoName, nullable(row.Branch), row.GitRef, nullable(row.CommitSHA), nullable(row.FinishedAt),
		nullable(row.Verdict), row.Closed, nullable(row.PanelRole), row.Content, row.ContentHash)
	if err != nil {
		return fmt.Errorf("upsert search mirror row: %w", err)
	}
	return nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// DeleteMissing removes documents absent from the canonical feed, including
// vector rows and stamps from every generation.
func (index *Index) DeleteMissing(ctx context.Context, seen map[string]struct{}) (int, error) {
	rows, err := index.db.QueryContext(ctx, `SELECT doc_key FROM review_mirror ORDER BY doc_key`)
	if err != nil {
		return 0, fmt.Errorf("list mirrored documents: %w", err)
	}
	var missing []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan mirrored document: %w", err)
		}
		if _, ok := seen[key]; !ok {
			missing = append(missing, key)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("scan mirrored documents: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close mirrored documents: %w", err)
	}

	for _, key := range missing {
		if err := index.vectors.DeleteVectors(ctx, key); err != nil {
			return 0, fmt.Errorf("delete vectors for %s: %w", key, err)
		}
	}
	tx, err := index.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin missing mirror deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range missing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_fts WHERE doc_key = ?`, key); err != nil {
			return 0, fmt.Errorf("delete missing FTS row: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_mirror WHERE doc_key = ?`, key); err != nil {
			return 0, fmt.Errorf("delete missing mirror row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit missing mirror deletion: %w", err)
	}
	return len(missing), nil
}
