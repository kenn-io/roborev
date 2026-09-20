package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// GenerationInfo is the persisted lifecycle state for one vector space.
type GenerationInfo struct {
	Key         string
	Fingerprint string
	Dimensions  int
	State       sqlitevec.State
}

// GenerationCounts describes current mirror coverage for a generation.
type GenerationCounts struct {
	Embedded int64
	Skipped  int64
	Backlog  int64
}

// EnsureGeneration creates a building generation for model when needed.
func (index *Index) EnsureGeneration(ctx context.Context, model vector.Generation) (string, error) {
	key := model.Fingerprint()
	var existing string
	err := index.db.QueryRowContext(ctx,
		`SELECT gen_key FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&existing)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("find search vector generation: %w", err)
	}
	if err := index.vectors.EnsureGeneration(ctx, key, model, sqlitevec.StateBuilding); err != nil {
		return "", fmt.Errorf("ensure search vector generation: %w", err)
	}
	return key, nil
}

// ActiveGeneration returns the newest active vector generation.
func (index *Index) ActiveGeneration(ctx context.Context) (GenerationInfo, bool, error) {
	var info GenerationInfo
	var state string
	err := index.db.QueryRowContext(ctx, `
		SELECT gen_key, fingerprint, dimension, state
		  FROM review_vectors_generations
		 WHERE state = ? ORDER BY ordinal DESC LIMIT 1`, string(sqlitevec.StateActive)).Scan(
		&info.Key, &info.Fingerprint, &info.Dimensions, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerationInfo{}, false, nil
	}
	if err != nil {
		return GenerationInfo{}, false, fmt.Errorf("read active search vector generation: %w", err)
	}
	info.State = sqlitevec.State(state)
	return info, true, nil
}

// GenerationAvailable reports whether fingerprint is the active vector space.
func (index *Index) GenerationAvailable(ctx context.Context, fingerprint string) (bool, error) {
	active, ok, err := index.ActiveGeneration(ctx)
	return ok && active.Fingerprint == fingerprint, err
}

// PendingGeneration returns a stable page of documents awaiting a generation.
func (index *Index) PendingGeneration(ctx context.Context, key string, limit int) ([]vector.Pending[string], error) {
	return index.vectors.PendingForGeneration(ctx, key, limit)
}

// SaveGenerationVectors atomically saves vectors for the revision that was read.
func (index *Index) SaveGenerationVectors(
	ctx context.Context, key string, pending vector.Pending[string], vectors []vector.ChunkVector,
) error {
	return index.vectors.SaveVectors(ctx, key, pending.Doc, pending.Revision, vectors)
}

// semanticHit binds a kit hit to the mirror revision observed just after the
// vector query. Kit's Hit does not carry a revision, so this snapshot is what
// stops a later mirror update from inheriting the previous vector score.
type semanticHit struct {
	vector.Hit[string]
	ContentHash string
}

// QueryGeneration returns chunk-level hits from kit's sqlitevec store, with
// the current mirror revision snapshotted for each document.
func (index *Index) QueryGeneration(
	ctx context.Context, key string, query vector.Vector, limit int,
) ([]semanticHit, error) {
	hits, err := index.vectors.QueryGeneration(ctx, key, query, limit)
	if err != nil {
		return nil, fmt.Errorf("query semantic generation: %w", err)
	}
	out := make([]semanticHit, 0, len(hits))
	for _, hit := range hits {
		hash, ok, err := index.mirrorRevision(ctx, hit.Doc)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, semanticHit{Hit: hit, ContentHash: hash})
	}
	return out, nil
}

// GenerationCounts returns fresh embedded, intentionally skipped, and pending counts.
func (index *Index) GenerationCounts(ctx context.Context, key string) (GenerationCounts, error) {
	return index.generationCounts(ctx, index.db, key)
}

type generationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (index *Index) generationCounts(ctx context.Context, queryer generationQueryer, key string) (GenerationCounts, error) {
	var ordinal int64
	if err := queryer.QueryRowContext(ctx,
		`SELECT ordinal FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&ordinal); err != nil {
		return GenerationCounts{}, fmt.Errorf("find search vector generation counts: %w", err)
	}
	var counts GenerationCounts
	err := queryer.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN s.doc_key IS NOT NULL AND c.doc_key IS NOT NULL THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN s.doc_key IS NOT NULL AND c.doc_key IS NULL THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN s.doc_key IS NULL THEN 1 ELSE 0 END), 0)
		FROM review_mirror m
		LEFT JOIN review_vectors_stamps s
		  ON s.ordinal = ? AND s.doc_key = m.doc_key AND s.revision IS m.content_hash
		LEFT JOIN (SELECT DISTINCT ordinal, doc_key FROM review_vectors_chunks) c
		  ON c.ordinal = ? AND c.doc_key = m.doc_key`, ordinal, ordinal).Scan(
		&counts.Embedded, &counts.Skipped, &counts.Backlog)
	if err != nil {
		return GenerationCounts{}, fmt.Errorf("count search vector generation coverage: %w", err)
	}
	return counts, nil
}

// ActivateGeneration cuts over only after every mirrored document is covered,
// then reclaims all retired vector tables.
func (index *Index) ActivateGeneration(ctx context.Context, key string) error {
	tx, err := index.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search vector cutover: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := index.activateGenerationTx(ctx, tx, key); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search vector cutover: %w", err)
	}
	return nil
}

func (index *Index) activateGenerationTx(ctx context.Context, tx *sql.Tx, key string) error {
	counts, err := index.generationCounts(ctx, tx, key)
	if err != nil {
		return err
	}
	if counts.Backlog != 0 {
		return fmt.Errorf("activate search vector generation %s: %d documents remain", key, counts.Backlog)
	}

	var wantedOrdinal int64
	if err := tx.QueryRowContext(ctx,
		`SELECT ordinal FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&wantedOrdinal); err != nil {
		return fmt.Errorf("find search vector cutover generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE review_vectors_generations
		   SET state = CASE WHEN gen_key = ? THEN ? ELSE ? END
		 WHERE state IN (?, ?)`, key, string(sqlitevec.StateActive), string(sqlitevec.StateRetired),
		string(sqlitevec.StateActive), string(sqlitevec.StateBuilding)); err != nil {
		return fmt.Errorf("mark search vector generation cutover: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT ordinal FROM review_vectors_generations WHERE state = ? AND ordinal != ?`,
		string(sqlitevec.StateRetired), wantedOrdinal)
	if err != nil {
		return fmt.Errorf("list retired search vector generations: %w", err)
	}
	var retired []int64
	for rows.Next() {
		var ordinal int64
		if err := rows.Scan(&ordinal); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan retired search vector generation: %w", err)
		}
		retired = append(retired, ordinal)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan retired search vector generations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close retired search vector generations: %w", err)
	}
	for _, ordinal := range retired {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP TABLE review_vectors_v%d`, ordinal)); err != nil {
			return fmt.Errorf("drop retired search vector generation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_vectors_chunks WHERE ordinal = ?`, ordinal); err != nil {
			return fmt.Errorf("delete retired search vector chunks: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_vectors_stamps WHERE ordinal = ?`, ordinal); err != nil {
			return fmt.Errorf("delete retired search vector stamps: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_vectors_generations WHERE ordinal = ?`, ordinal); err != nil {
			return fmt.Errorf("delete retired search vector generation: %w", err)
		}
	}
	return nil
}

func (index *Index) mirrorRevision(ctx context.Context, docKey string) (string, bool, error) {
	var revision string
	err := index.db.QueryRowContext(ctx,
		`SELECT content_hash FROM review_mirror WHERE doc_key = ?`, docKey).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read search mirror revision: %w", err)
	}
	return revision, true, nil
}

func (index *Index) mirrorCount(ctx context.Context) (int64, error) {
	var count int64
	if err := index.db.QueryRowContext(ctx, `SELECT count(*) FROM review_mirror`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count search mirror: %w", err)
	}
	return count, nil
}
