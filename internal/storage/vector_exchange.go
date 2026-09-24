package storage

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// The vector exchange is a shared cache of review-search embeddings kept in
// the sync PostgreSQL database. Every syncing daemon with embeddings
// configured looks vectors up here before calling its provider, and publishes
// the vectors it computes. Records are keyed by review UUID, generation
// fingerprint and the SHA-256 of the exact embedded text, so a record is only
// ever used for byte-identical input in an identical vector space.

var (
	// ErrVectorExchangeUnreachable reports that the sync database cannot be
	// queried right now (disconnected, network failure, closed pool).
	ErrVectorExchangeUnreachable = errors.New("vector exchange unreachable")
	// ErrVectorExchangeUnsupported reports that the exchange tables do not
	// exist and could not be created (read-only or restricted database role).
	ErrVectorExchangeUnsupported = errors.New("vector exchange tables are absent")
)

// Vector record statuses.
const (
	VectorStatusOK      = "ok"
	VectorStatusSkipped = "skipped"
)

// VectorKey identifies one exact embedded text of one review.
type VectorKey struct {
	ReviewUUID    string
	ContentSHA256 string
}

// VectorChunk is one chunk vector of a review document.
type VectorChunk struct {
	Index  int
	Vector []float32
}

// VectorRecord is one review's vectors for one generation and one text.
// Status "skipped" records carry no chunks: the provider rejected the text.
// Malformed is set by Lookup when the stored arrays cannot be decoded.
type VectorRecord struct {
	Key       VectorKey
	Status    string
	Dims      int
	Chunks    []VectorChunk
	Malformed bool
}

// VectorGeneration is the descriptor stored in embedding_generations.
type VectorGeneration struct {
	Fingerprint string            `json:"fingerprint"`
	Model       string            `json:"model"`
	Dimensions  int               `json:"dimensions"`
	Params      map[string]string `json:"params"`
}

// VectorExchange runs exchange queries on the sync worker's current pool.
type VectorExchange struct {
	pool      func() *PgPool
	machineID string
}

// NewVectorExchange binds the exchange to a pool accessor. pool may return
// nil while the sync worker is disconnected.
func NewVectorExchange(pool func() *PgPool, machineID string) *VectorExchange {
	return &VectorExchange{pool: pool, machineID: machineID}
}

func (e *VectorExchange) current() (*PgPool, error) {
	if e == nil || e.pool == nil {
		return nil, ErrVectorExchangeUnreachable
	}
	pool := e.pool()
	if pool == nil {
		return nil, ErrVectorExchangeUnreachable
	}
	return pool, nil
}

func vectorExchangeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrVectorExchangeUnsupported) || errors.Is(err, ErrVectorExchangeUnreachable) {
		return err
	}
	if code, ok := isPgError(err); ok && code == "42P01" {
		return fmt.Errorf("%w: %w", ErrVectorExchangeUnsupported, err)
	}
	return fmt.Errorf("%w: %w", ErrVectorExchangeUnreachable, err)
}

// Target returns the sync database identifier, proving the exchange is
// reachable and its tables exist.
func (e *VectorExchange) Target(ctx context.Context) (string, error) {
	pool, err := e.current()
	if err != nil {
		return "", err
	}
	present, err := pool.vectorExchangePresent(ctx)
	if err != nil {
		return "", vectorExchangeError(err)
	}
	if !present {
		return "", ErrVectorExchangeUnsupported
	}
	id, err := pool.GetDatabaseID(ctx)
	if err != nil {
		return "", vectorExchangeError(err)
	}
	return id.String(), nil //nolint:forbidigo // exchange target is an opaque TEXT token.
}

// TouchGeneration records that this machine uses gen, for garbage collection.
// It reports whether the generation row was recreated after garbage collection.
func (e *VectorExchange) TouchGeneration(ctx context.Context, gen VectorGeneration) (bool, error) {
	pool, err := e.current()
	if err != nil {
		return false, err
	}
	descriptor, err := json.Marshal(gen)
	if err != nil {
		return false, fmt.Errorf("encode vector generation: %w", err)
	}
	var created bool
	err = pool.pool.QueryRow(ctx, `
		INSERT INTO embedding_generations (fingerprint, descriptor, last_machine_id, last_used_at)
		VALUES ($1, $2::jsonb, $3, NOW())
		ON CONFLICT (fingerprint) DO UPDATE SET
			descriptor = EXCLUDED.descriptor,
			last_machine_id = EXCLUDED.last_machine_id,
			last_used_at = NOW()
		RETURNING (xmax = 0)`,
		gen.Fingerprint, string(descriptor), e.machineID).Scan(&created)
	if err != nil {
		return false, vectorExchangeError(err)
	}
	return created, nil
}

// Lookup returns the records that exactly match keys under fingerprint.
// Keys without a record are simply absent from the result.
func (e *VectorExchange) Lookup(ctx context.Context, fingerprint string, keys []VectorKey) ([]VectorRecord, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pool, err := e.current()
	if err != nil {
		return nil, err
	}
	uuids, hashes := splitVectorKeys(keys)
	rows, err := pool.pool.Query(ctx, `
		SELECT e.review_uuid, e.content_sha256, e.status, e.dims, e.chunk_indexes, e.chunks
		  FROM unnest($2::text[], $3::text[]) AS w(review_uuid, content_sha256)
		  JOIN review_embeddings e
		    ON e.review_uuid = w.review_uuid
		   AND e.content_sha256 = w.content_sha256
		   AND e.generation_fingerprint = $1`,
		fingerprint, uuids, hashes)
	if err != nil {
		return nil, vectorExchangeError(err)
	}
	defer rows.Close()
	var records []VectorRecord
	for rows.Next() {
		var record VectorRecord
		var indexes []int32
		var chunks [][]byte
		if err := rows.Scan(&record.Key.ReviewUUID, &record.Key.ContentSHA256, &record.Status,
			&record.Dims, &indexes, &chunks); err != nil {
			return nil, vectorExchangeError(err)
		}
		record.Chunks, record.Malformed = decodeVectorChunks(indexes, chunks)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, vectorExchangeError(err)
	}
	return records, nil
}

// Claim asks to become the single embedder of key under fingerprint for ttl.
// It succeeds only when no record exists for key and no other machine holds
// an unexpired claim. Re-claiming a claim this machine holds renews it.
func (e *VectorExchange) Claim(ctx context.Context, fingerprint string, key VectorKey, ttl time.Duration) (bool, error) {
	pool, err := e.current()
	if err != nil {
		return false, err
	}
	var holder string
	err = pool.pool.QueryRow(ctx, `
		INSERT INTO review_embedding_claims
			(review_uuid, generation_fingerprint, content_sha256, machine_id, expires_at)
		SELECT $1, $2, $3, $4, NOW() + make_interval(secs => $5)
		 WHERE NOT EXISTS (
			SELECT 1 FROM review_embeddings e
			 WHERE e.review_uuid = $1 AND e.generation_fingerprint = $2 AND e.content_sha256 = $3)
		ON CONFLICT (review_uuid, generation_fingerprint, content_sha256) DO UPDATE SET
			machine_id = EXCLUDED.machine_id,
			expires_at = EXCLUDED.expires_at
		 WHERE review_embedding_claims.expires_at <= NOW()
		    OR review_embedding_claims.machine_id = EXCLUDED.machine_id
		RETURNING machine_id`,
		key.ReviewUUID, fingerprint, key.ContentSHA256, e.machineID, ttl.Seconds()).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, vectorExchangeError(err)
	}
	return holder == e.machineID, nil
}

// Publish inserts records under fingerprint without replacing an existing
// exact-key result. A discarded record or collected generation can be
// republished because its key is absent.
func (e *VectorExchange) Publish(ctx context.Context, fingerprint string, records []VectorRecord) error {
	if len(records) == 0 {
		return nil
	}
	pool, err := e.current()
	if err != nil {
		return err
	}
	tx, err := pool.pool.Begin(ctx)
	if err != nil {
		return vectorExchangeError(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, record := range records {
		indexes := make([]int32, len(record.Chunks))
		chunks := make([][]byte, len(record.Chunks))
		for i, chunk := range record.Chunks {
			indexes[i] = int32(chunk.Index)
			chunks[i] = EncodeVectorChunk(chunk.Vector)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO review_embeddings
				(review_uuid, generation_fingerprint, content_sha256, status, dims,
				 chunk_indexes, chunks, publisher_machine_id, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
			ON CONFLICT (review_uuid, generation_fingerprint, content_sha256) DO NOTHING`,
			record.Key.ReviewUUID, fingerprint, record.Key.ContentSHA256, record.Status, record.Dims,
			indexes, chunks, e.machineID)
		if err != nil {
			return vectorExchangeError(err)
		}
	}
	return vectorExchangeError(tx.Commit(ctx))
}

// Release deletes this machine's claims on keys.
func (e *VectorExchange) Release(ctx context.Context, fingerprint string, keys []VectorKey) error {
	if len(keys) == 0 {
		return nil
	}
	pool, err := e.current()
	if err != nil {
		return err
	}
	uuids, hashes := splitVectorKeys(keys)
	_, err = pool.pool.Exec(ctx, `
		DELETE FROM review_embedding_claims c
		 USING unnest($3::text[], $4::text[]) AS w(review_uuid, content_sha256)
		 WHERE c.generation_fingerprint = $1 AND c.machine_id = $2
		   AND c.review_uuid = w.review_uuid AND c.content_sha256 = w.content_sha256`,
		fingerprint, e.machineID, uuids, hashes)
	return vectorExchangeError(err)
}

// Discard deletes one record that failed validation so it can be re-embedded.
func (e *VectorExchange) Discard(ctx context.Context, fingerprint string, key VectorKey) error {
	pool, err := e.current()
	if err != nil {
		return err
	}
	_, err = pool.pool.Exec(ctx, `
		DELETE FROM review_embeddings
		 WHERE review_uuid = $1 AND generation_fingerprint = $2 AND content_sha256 = $3`,
		key.ReviewUUID, fingerprint, key.ContentSHA256)
	return vectorExchangeError(err)
}

// CollectGarbage removes generations no daemon has touched for unusedFor,
// their records, and claims expired for more than a day.
// It returns the number of generations removed.
func (e *VectorExchange) CollectGarbage(ctx context.Context, unusedFor time.Duration) (int64, error) {
	pool, err := e.current()
	if err != nil {
		return 0, err
	}
	var removed int64
	err = pool.pool.QueryRow(ctx, `
		WITH stale AS (
			DELETE FROM embedding_generations
			 WHERE last_used_at < NOW() - make_interval(secs => $1)
			RETURNING fingerprint
		), dropped_records AS (
			DELETE FROM review_embeddings
			 WHERE generation_fingerprint IN (SELECT fingerprint FROM stale)
			RETURNING 1
		), dropped_claims AS (
			DELETE FROM review_embedding_claims
			 WHERE generation_fingerprint IN (SELECT fingerprint FROM stale)
			    OR expires_at < NOW() - INTERVAL '1 day'
			RETURNING 1
		)
		SELECT count(*) FROM stale`,
		unusedFor.Seconds()).Scan(&removed)
	if err != nil {
		return 0, vectorExchangeError(err)
	}
	return removed, nil
}

func splitVectorKeys(keys []VectorKey) ([]string, []string) {
	uuids := make([]string, len(keys))
	hashes := make([]string, len(keys))
	for i, key := range keys {
		uuids[i] = key.ReviewUUID
		hashes[i] = key.ContentSHA256
	}
	return uuids, hashes
}

// EncodeVectorChunk serializes a vector as little-endian float32 bytes, the
// same layout sqlite-vec stores.
func EncodeVectorChunk(vector []float32) []byte {
	out := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(value))
	}
	return out
}

// DecodeVectorChunk parses little-endian float32 bytes.
func DecodeVectorChunk(raw []byte) ([]float32, bool) {
	if len(raw)%4 != 0 {
		return nil, false
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return out, true
}

func decodeVectorChunks(indexes []int32, chunks [][]byte) ([]VectorChunk, bool) {
	if len(indexes) != len(chunks) {
		return nil, true
	}
	out := make([]VectorChunk, len(chunks))
	for i, raw := range chunks {
		vector, ok := DecodeVectorChunk(raw)
		if !ok {
			return nil, true
		}
		out[i] = VectorChunk{Index: int(indexes[i]), Vector: vector}
	}
	return out, false
}
