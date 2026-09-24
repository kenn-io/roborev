package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/storage"
)

// Sidecar bookkeeping for the shared PostgreSQL vector cache. review_exchange
// holds one row per shared document (share_state > 0) for the generation and
// text the daemon last saw pending. A row whose gen_key or content_hash no
// longer matches the mirror is stale and is reset by observeSharedPending.

// Shared-vector source statuses reported in health.
const (
	SourceDisabled    = "disabled"
	SourceOK          = "ok"
	SourceUnsupported = "unsupported"
	SourceUnreachable = "unreachable"
)

// Shared-vector constants. They are deliberately not user settings.
const (
	exchangeLookupBatch       = 64
	exchangeTimeout           = 30 * time.Second
	defaultPeerClaimDelay     = 2 * time.Minute
	defaultClaimTTL           = 10 * time.Minute
	defaultLocalFallbackAfter = 24 * time.Hour
	defaultLookupMinBackoff   = 30 * time.Second
	exchangeLookupMaxBackoff  = 30 * time.Minute
	exchangeTouchInterval     = time.Hour
	exchangeGCInterval        = 24 * time.Hour
	exchangeUnusedGeneration  = 30 * 24 * time.Hour
)

const (
	exchangeOriginPeer  = "peer"
	exchangeOriginLocal = "local"
)

// notCoveredSQL must stay identical to NOT kit sqlitevec coveredPredicate for
// review_mirror alias m and review_vectors_stamps alias s, so a document is
// exactly one of pending or searchable.
const notCoveredSQL = `NOT (m.embed_gen IS NOT NULL AND s.doc_key IS NOT NULL AND m.content_hash IS s.revision)`

// sharedPendingFrom joins the mirror to the stamps of generation ordinal ?1
// and to the exchange row for generation key ?2 and the current text.
const sharedPendingFrom = `
  FROM review_mirror m
  LEFT JOIN review_vectors_stamps s ON s.ordinal = ? AND s.doc_key = m.doc_key
  LEFT JOIN review_exchange x
    ON x.doc_key = m.doc_key AND x.gen_key = ? AND x.content_hash = m.content_hash`

// localFillSQL selects pending documents this daemon may embed with its own
// provider: local-only documents, shared documents past fallback, and claimed
// documents only while the exchange is reachable.
// Parameters: fallback cutoff, allow claims, now.
const localFillSQL = `(m.share_state = 0
    OR COALESCE(x.first_pending_at, 9223372036854775807) <= ?
    OR (? AND COALESCE(x.claim_expires_at, 0) > ?))`

type exchangeCandidate struct {
	DocKey         string
	ContentHash    string
	ShareState     storage.SearchShareState
	FirstPendingAt time.Time
	Attempts       int
}

// ExchangeCounts summarizes shared-vector state for one generation.
type ExchangeCounts struct {
	Imported     int64
	Published    int64
	AwaitingPeer int64
	ClaimsHeld   int64
}

func (index *Index) generationOrdinal(ctx context.Context, key string) (int64, error) {
	var ordinal int64
	if err := index.db.QueryRowContext(ctx,
		`SELECT ordinal FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&ordinal); err != nil {
		return 0, fmt.Errorf("find search vector generation: %w", err)
	}
	return ordinal, nil
}

// observeSharedPending records every pending shared document of key and
// resets rows whose generation or text changed since they were written.
func (index *Index) observeSharedPending(ctx context.Context, key string, now time.Time) error {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return err
	}
	_, err = index.db.ExecContext(ctx, `
		INSERT INTO review_exchange (
			doc_key, gen_key, content_hash, first_pending_at, next_attempt_at,
			attempts, claim_expires_at, origin, published_target)
		SELECT m.doc_key, ?, m.content_hash, ?, ?, 0, 0, '', ''
		  FROM review_mirror m
		  LEFT JOIN review_vectors_stamps s ON s.ordinal = ? AND s.doc_key = m.doc_key
		 WHERE m.share_state > 0 AND `+notCoveredSQL+`
		ON CONFLICT(doc_key) DO UPDATE SET
			gen_key = excluded.gen_key,
			content_hash = excluded.content_hash,
			first_pending_at = excluded.first_pending_at,
			next_attempt_at = excluded.next_attempt_at,
			attempts = 0, claim_expires_at = 0, origin = '', published_target = ''
		 WHERE review_exchange.gen_key != excluded.gen_key
		    OR review_exchange.content_hash != excluded.content_hash`,
		key, now.Unix(), now.Unix(), ordinal)
	if err != nil {
		return fmt.Errorf("observe shared search documents: %w", err)
	}
	return nil
}

// dueSharedCandidates returns pending shared documents whose next lookup is
// due, that this daemon does not hold a claim on, and whose local fallback
// has not started.
func (index *Index) dueSharedCandidates(
	ctx context.Context, key string, now, fallbackCutoff time.Time, limit int,
) ([]exchangeCandidate, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := index.db.QueryContext(ctx, `
		SELECT m.doc_key, m.content_hash, m.share_state, x.first_pending_at, x.attempts`+
		sharedPendingFrom+`
		 WHERE m.share_state > 0 AND `+notCoveredSQL+`
		   AND x.doc_key IS NOT NULL
		   AND x.next_attempt_at <= ?
		   AND x.claim_expires_at <= ?
		   AND x.first_pending_at > ?
		 ORDER BY x.next_attempt_at, m.doc_key
		 LIMIT ?`,
		ordinal, key, now.Unix(), now.Unix(), fallbackCutoff.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("list due shared search documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []exchangeCandidate
	for rows.Next() {
		var candidate exchangeCandidate
		var shareState int
		var firstPending int64
		if err := rows.Scan(&candidate.DocKey, &candidate.ContentHash, &shareState,
			&firstPending, &candidate.Attempts); err != nil {
			return nil, fmt.Errorf("scan due shared search document: %w", err)
		}
		candidate.ShareState = storage.SearchShareState(shareState)
		candidate.FirstPendingAt = time.Unix(firstPending, 0)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// recordLookup counts one lookup attempt and schedules the next.
func (index *Index) recordLookup(ctx context.Context, key string, candidate exchangeCandidate, next time.Time) error {
	_, err := index.db.ExecContext(ctx, `
		UPDATE review_exchange SET attempts = attempts + 1, next_attempt_at = ?
		 WHERE doc_key = ? AND gen_key = ? AND content_hash = ?`,
		next.Unix(), candidate.DocKey, key, candidate.ContentHash)
	if err != nil {
		return fmt.Errorf("record shared vector lookup: %w", err)
	}
	return nil
}

// recordClaim counts one attempt and remembers that this daemon holds the
// PostgreSQL claim until expires.
func (index *Index) recordClaim(ctx context.Context, key string, candidate exchangeCandidate, expires time.Time) error {
	_, err := index.db.ExecContext(ctx, `
		UPDATE review_exchange
		   SET attempts = attempts + 1, claim_expires_at = ?, next_attempt_at = ?
		 WHERE doc_key = ? AND gen_key = ? AND content_hash = ?`,
		expires.Unix(), expires.Unix(), candidate.DocKey, key, candidate.ContentHash)
	if err != nil {
		return fmt.Errorf("record shared vector claim: %w", err)
	}
	return nil
}

// recordExchanged marks the (generation, text) of doc as present in the
// exchange identified by target, either imported from a peer or published.
func (index *Index) recordExchanged(ctx context.Context, key, doc, contentHash, origin, target string, now time.Time) error {
	_, err := index.db.ExecContext(ctx, `
		INSERT INTO review_exchange (
			doc_key, gen_key, content_hash, first_pending_at, next_attempt_at,
			attempts, claim_expires_at, origin, published_target)
		VALUES (?, ?, ?, ?, ?, 1, 0, ?, ?)
		ON CONFLICT(doc_key) DO UPDATE SET
			gen_key = excluded.gen_key,
			content_hash = excluded.content_hash,
			attempts = MAX(review_exchange.attempts, 1),
			origin = excluded.origin,
			published_target = excluded.published_target,
			first_pending_at = CASE
				WHEN review_exchange.gen_key = excluded.gen_key
				 AND review_exchange.content_hash = excluded.content_hash
				THEN review_exchange.first_pending_at ELSE excluded.first_pending_at END,
			claim_expires_at = CASE
				WHEN review_exchange.gen_key = excluded.gen_key
				 AND review_exchange.content_hash = excluded.content_hash
				THEN review_exchange.claim_expires_at ELSE 0 END`,
		doc, key, contentHash, now.Unix(), now.Unix(), origin, target)
	if err != nil {
		return fmt.Errorf("record exchanged search vectors: %w", err)
	}
	return nil
}

// forgetPublishedTarget clears publication markers after the exchange reports
// that it recreated a generation whose cached records were garbage-collected.
func (index *Index) forgetPublishedTarget(ctx context.Context, key, target string) error {
	_, err := index.db.ExecContext(ctx, `
		UPDATE review_exchange SET published_target = ''
		 WHERE gen_key = ? AND published_target = ?`, key, target)
	if err != nil {
		return fmt.Errorf("forget published shared search vectors: %w", err)
	}
	return nil
}

type publishCandidate struct {
	DocKey      string
	ContentHash string
}

// publishCandidates returns covered shared documents of key whose vectors
// are not yet known to be in the exchange identified by target.
func (index *Index) publishCandidates(ctx context.Context, key, target string, limit int) ([]publishCandidate, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := index.db.QueryContext(ctx, `
		SELECT m.doc_key, m.content_hash
		  FROM review_mirror m
		  JOIN review_vectors_stamps s ON s.ordinal = ? AND s.doc_key = m.doc_key
		  LEFT JOIN review_exchange x ON x.doc_key = m.doc_key
		 WHERE m.share_state > 0
		   AND m.embed_gen IS NOT NULL AND m.content_hash IS s.revision
		   AND (x.doc_key IS NULL OR x.gen_key != ? OR x.content_hash != m.content_hash
		        OR x.published_target != ?)
		 ORDER BY m.doc_key
		 LIMIT ?`, ordinal, key, target, limit)
	if err != nil {
		return nil, fmt.Errorf("list publishable search vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []publishCandidate
	for rows.Next() {
		var candidate publishCandidate
		if err := rows.Scan(&candidate.DocKey, &candidate.ContentHash); err != nil {
			return nil, fmt.Errorf("scan publishable search vector: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// readChunkVectors returns doc's stored chunk vectors for key, ordered by
// chunk index. A stamped document with no chunks returns an empty slice.
func (index *Index) readChunkVectors(ctx context.Context, key, doc string) ([]storage.VectorChunk, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := index.db.QueryContext(ctx, `
		SELECT chunk_index, vec_rowid FROM review_vectors_chunks
		 WHERE ordinal = ? AND doc_key = ? ORDER BY chunk_index`, ordinal, doc)
	if err != nil {
		return nil, fmt.Errorf("read search vector chunks: %w", err)
	}
	type chunkRef struct {
		index int
		rowid int64
	}
	var refs []chunkRef
	for rows.Next() {
		var ref chunkRef
		if err := rows.Scan(&ref.index, &ref.rowid); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan search vector chunk: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read search vector chunks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close search vector chunks: %w", err)
	}
	chunks := make([]storage.VectorChunk, 0, len(refs))
	for _, ref := range refs {
		var blob []byte
		if err := index.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT embedding FROM review_vectors_v%d WHERE rowid = ?`, ordinal),
			ref.rowid).Scan(&blob); err != nil {
			return nil, fmt.Errorf("read search vector: %w", err)
		}
		values, ok := storage.DecodeVectorChunk(blob)
		if !ok {
			return nil, fmt.Errorf("search vector %d of %s is not float32", ref.index, doc)
		}
		chunks = append(chunks, storage.VectorChunk{Index: ref.index, Vector: values})
	}
	return chunks, nil
}

// finishedClaims returns claimed documents that are now covered for key.
func (index *Index) finishedClaims(ctx context.Context, key string) ([]publishCandidate, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return nil, err
	}
	rows, err := index.db.QueryContext(ctx, `
		SELECT m.doc_key, m.content_hash`+sharedPendingFrom+`
		 WHERE x.claim_expires_at > 0 AND NOT (`+notCoveredSQL+`)`, ordinal, key)
	if err != nil {
		return nil, fmt.Errorf("list finished shared vector claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var finished []publishCandidate
	for rows.Next() {
		var candidate publishCandidate
		if err := rows.Scan(&candidate.DocKey, &candidate.ContentHash); err != nil {
			return nil, fmt.Errorf("scan finished shared vector claim: %w", err)
		}
		finished = append(finished, candidate)
	}
	return finished, rows.Err()
}

func (index *Index) clearClaim(ctx context.Context, key, doc, contentHash string) error {
	_, err := index.db.ExecContext(ctx, `
		UPDATE review_exchange SET claim_expires_at = 0
		 WHERE doc_key = ? AND gen_key = ? AND content_hash = ?`, doc, key, contentHash)
	if err != nil {
		return fmt.Errorf("clear shared vector claim: %w", err)
	}
	return nil
}

// localFillBacklog counts pending documents the daemon may embed itself now.
func (index *Index) localFillBacklog(ctx context.Context, key string, now, fallbackCutoff time.Time, allowClaimed bool) (int64, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return 0, err
	}
	var count int64
	err = index.db.QueryRowContext(ctx, `SELECT count(*)`+sharedPendingFrom+`
		 WHERE `+notCoveredSQL+` AND `+localFillSQL,
		ordinal, key, fallbackCutoff.Unix(), allowClaimed, now.Unix()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count locally embeddable search documents: %w", err)
	}
	return count, nil
}

// nextExchangeDue returns the earliest future time a shared document needs
// attention (next lookup, claim expiry, or local fallback), or zero.
func (index *Index) nextExchangeDue(ctx context.Context, key string, fallbackAfter time.Duration) (time.Time, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return time.Time{}, err
	}
	var due sql.NullInt64
	err = index.db.QueryRowContext(ctx, `
		SELECT MIN(MIN(x.next_attempt_at, x.first_pending_at + ?))`+sharedPendingFrom+`
		 WHERE m.share_state > 0 AND `+notCoveredSQL+` AND x.doc_key IS NOT NULL`,
		int64(fallbackAfter/time.Second), ordinal, key).Scan(&due)
	if err != nil {
		return time.Time{}, fmt.Errorf("find next shared vector attempt: %w", err)
	}
	if !due.Valid {
		return time.Time{}, nil
	}
	return time.Unix(due.Int64, 0), nil
}

// ExchangeCounts reports shared-vector coverage for key.
func (index *Index) ExchangeCounts(ctx context.Context, key string, now, fallbackCutoff time.Time) (ExchangeCounts, error) {
	ordinal, err := index.generationOrdinal(ctx, key)
	if err != nil {
		return ExchangeCounts{}, err
	}
	var counts ExchangeCounts
	err = index.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN x.origin = 'peer' AND NOT (`+notCoveredSQL+`) THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN x.origin = 'local' AND x.published_target != ''
		                     AND NOT (`+notCoveredSQL+`) THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN m.share_state > 0 AND `+notCoveredSQL+`
		                     AND COALESCE(x.claim_expires_at, 0) <= ?
		                     AND COALESCE(x.first_pending_at, 9223372036854775807) > ? THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN COALESCE(x.claim_expires_at, 0) > ? AND `+notCoveredSQL+` THEN 1 ELSE 0 END), 0)`+
		sharedPendingFrom,
		now.Unix(), fallbackCutoff.Unix(), now.Unix(), ordinal, key).Scan(
		&counts.Imported, &counts.Published, &counts.AwaitingPeer, &counts.ClaimsHeld,
	)
	if err != nil {
		return ExchangeCounts{}, fmt.Errorf("count shared search vectors: %w", err)
	}
	return counts, nil
}

// sharedActivationBlockers counts documents that must be handled before a
// sharing daemon activates key: pending local documents, shared documents
// never looked up, claimed documents, and shared documents due for fallback.
func (index *Index) sharedActivationBlockers(
	ctx context.Context, queryer generationQueryer, key string, now, fallbackCutoff time.Time,
) (int64, error) {
	var ordinal int64
	if err := queryer.QueryRowContext(ctx,
		`SELECT ordinal FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&ordinal); err != nil {
		return 0, fmt.Errorf("find search vector generation: %w", err)
	}
	var blockers int64
	err := queryer.QueryRowContext(ctx, `SELECT count(*)`+sharedPendingFrom+`
		 WHERE `+notCoveredSQL+`
		   AND (m.share_state = 0 OR x.doc_key IS NULL OR x.attempts = 0
		        OR x.claim_expires_at > ? OR x.first_pending_at <= ?)`,
		ordinal, key, now.Unix(), fallbackCutoff.Unix()).Scan(&blockers)
	if err != nil {
		return 0, fmt.Errorf("count shared activation blockers: %w", err)
	}
	return blockers, nil
}

// ActivateSharedGeneration activates key once every document that this
// daemon must handle itself is covered and every shared document has had at
// least one exchange attempt. Pending shared documents keep search partial.
func (index *Index) ActivateSharedGeneration(ctx context.Context, key string, now, fallbackCutoff time.Time) error {
	tx, err := index.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search vector cutover: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	blockers, err := index.sharedActivationBlockers(ctx, tx, key, now, fallbackCutoff)
	if err != nil {
		return err
	}
	if blockers != 0 {
		return fmt.Errorf("activate search vector generation %s: %d documents remain", key, blockers)
	}
	if err := index.cutoverGenerationTx(ctx, tx, key); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search vector cutover: %w", err)
	}
	return nil
}

// validateExchangeRecord converts a peer record into vectors for the local
// text and dimensions, or explains why it must be rejected.
func validateExchangeRecord(record storage.VectorRecord, content string, dims int) ([]vector.ChunkVector, error) {
	if record.Malformed {
		return nil, errors.New("record arrays are malformed")
	}
	if record.Dims != dims {
		return nil, fmt.Errorf("record has %d dimensions, generation expects %d", record.Dims, dims)
	}
	switch record.Status {
	case storage.VectorStatusSkipped:
		if len(record.Chunks) != 0 {
			return nil, errors.New("skipped record carries vectors")
		}
		return nil, nil
	case storage.VectorStatusOK:
	default:
		return nil, fmt.Errorf("record status %q is unknown", record.Status)
	}
	want := vector.Split(content, vector.SplitOptions{MaxRunes: ChunkMaxRunes, Overlap: ChunkOverlapRunes})
	if len(record.Chunks) == 0 || len(record.Chunks) != len(want) {
		return nil, fmt.Errorf("record has %d chunks, local text splits into %d", len(record.Chunks), len(want))
	}
	seen := make(map[int]struct{}, len(record.Chunks))
	out := make([]vector.ChunkVector, 0, len(record.Chunks))
	for i, chunk := range record.Chunks {
		if chunk.Index < 0 {
			return nil, fmt.Errorf("chunk index %d is negative", chunk.Index)
		}
		if _, dup := seen[chunk.Index]; dup {
			return nil, fmt.Errorf("chunk index %d repeats", chunk.Index)
		}
		seen[chunk.Index] = struct{}{}
		if chunk.Index != want[i].Index {
			return nil, fmt.Errorf("chunk %d has index %d, want %d", i, chunk.Index, want[i].Index)
		}
		if len(chunk.Vector) != dims {
			return nil, fmt.Errorf("chunk %d has %d dimensions, expected %d", chunk.Index, len(chunk.Vector), dims)
		}
		var sumSquares float64
		for _, value := range chunk.Vector {
			f := float64(value)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, fmt.Errorf("chunk %d is not finite", chunk.Index)
			}
			sumSquares += f * f
		}
		if norm := math.Sqrt(sumSquares); norm < 0.99 || norm > 1.01 {
			return nil, fmt.Errorf("chunk %d has norm %.4f outside [0.99, 1.01]", chunk.Index, norm)
		}
		out = append(out, vector.ChunkVector{ChunkIndex: chunk.Index, Vector: chunk.Vector})
	}
	return out, nil
}
