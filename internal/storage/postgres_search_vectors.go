package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
)

// Shared search vectors let syncing daemons reuse each other's review-search
// embeddings instead of each sending the same review text to its provider.
// A row holds one review's chunk vectors for one embedding space and one
// exact document text, so it only ever replaces a local encode of
// byte-identical input in an identical vector space.
//
// The table is created outside pgSchemaVersion on purpose: bumping the
// version would make older daemons refuse the database, while they ignore an
// extra table. Rows reference reviews, so a vector is only stored for a
// review whose text is already in the sync database.
const searchVectorsDDL = `CREATE TABLE IF NOT EXISTS review_search_vectors (
	review_uuid UUID NOT NULL REFERENCES reviews(uuid) ON DELETE CASCADE,
	space TEXT NOT NULL,
	content_sha256 TEXT NOT NULL,
	chunk_indexes INTEGER[] NOT NULL,
	vectors BYTEA NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (review_uuid, space, content_sha256)
)`

// SearchVectorKey identifies one exact document text of one review.
type SearchVectorKey struct {
	ReviewUUID    string
	ContentSHA256 string
}

// SearchVectorChunk is one chunk vector of a review document.
type SearchVectorChunk struct {
	Index  int
	Vector []float32
}

// ensureSearchVectorsSchema creates the shared vector table. A role without
// CREATE privilege only loses vector sharing, so failure is logged, not fatal.
func (p *PgPool) ensureSearchVectorsSchema(ctx context.Context) {
	if _, err := p.pool.Exec(ctx, searchVectorsDDL); err != nil {
		log.Printf("Sync: shared search vectors unavailable: %v", err)
	}
}

// LookupSearchVectors returns the stored vectors for each key found in space.
func (p *PgPool) LookupSearchVectors(
	ctx context.Context, space string, keys []SearchVectorKey,
) (map[SearchVectorKey][]SearchVectorChunk, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	uuids := make([]string, len(keys))
	hashes := make([]string, len(keys))
	for i, key := range keys {
		uuids[i] = key.ReviewUUID
		hashes[i] = key.ContentSHA256
	}
	rows, err := p.pool.Query(ctx, `
		SELECT v.review_uuid::text, v.content_sha256, v.chunk_indexes, v.vectors
		  FROM review_search_vectors v
		  JOIN unnest($2::uuid[], $3::text[]) AS k(review_uuid, content_sha256)
		    ON v.review_uuid = k.review_uuid AND v.content_sha256 = k.content_sha256
		 WHERE v.space = $1`, space, uuids, hashes)
	if err != nil {
		return nil, fmt.Errorf("lookup shared search vectors: %w", err)
	}
	defer rows.Close()

	found := make(map[SearchVectorKey][]SearchVectorChunk)
	for rows.Next() {
		var key SearchVectorKey
		var indexes []int32
		var blob []byte
		if err := rows.Scan(&key.ReviewUUID, &key.ContentSHA256, &indexes, &blob); err != nil {
			return nil, fmt.Errorf("scan shared search vectors: %w", err)
		}
		chunks, err := decodeSearchVectors(indexes, blob)
		if err != nil {
			// A malformed row is a miss; the caller embeds the text itself.
			continue
		}
		found[key] = chunks
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read shared search vectors: %w", err)
	}
	return found, nil
}

// PublishSearchVectors stores chunks for key in space. It reports false,
// without error, when the review is not in the sync database yet or the
// same text was already published.
func (p *PgPool) PublishSearchVectors(
	ctx context.Context, space string, key SearchVectorKey, chunks []SearchVectorChunk,
) (bool, error) {
	indexes, blob, err := encodeSearchVectors(chunks)
	if err != nil {
		return false, err
	}
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO review_search_vectors (review_uuid, space, content_sha256, chunk_indexes, vectors)
		SELECT $1::uuid, $2, $3, $4, $5
		 WHERE EXISTS (SELECT 1 FROM reviews WHERE uuid = $1::uuid)
		ON CONFLICT DO NOTHING`, key.ReviewUUID, space, key.ContentSHA256, indexes, blob)
	if err != nil {
		if code, ok := isPgError(err); ok && code == "23503" {
			// The review was deleted between the existence check and the insert.
			return false, nil
		}
		return false, fmt.Errorf("publish shared search vectors: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func encodeSearchVectors(chunks []SearchVectorChunk) ([]int32, []byte, error) {
	if len(chunks) == 0 {
		return nil, nil, errors.New("shared search vectors need at least one chunk")
	}
	dims := len(chunks[0].Vector)
	indexes := make([]int32, len(chunks))
	blob := make([]byte, 0, len(chunks)*dims*4)
	for i, chunk := range chunks {
		if len(chunk.Vector) != dims || dims == 0 {
			return nil, nil, fmt.Errorf("shared search vector chunk %d has %d dimensions, expected %d",
				chunk.Index, len(chunk.Vector), dims)
		}
		indexes[i] = int32(chunk.Index) //nolint:gosec // chunk indexes are small non-negative counts
		for _, value := range chunk.Vector {
			blob = binary.LittleEndian.AppendUint32(blob, math.Float32bits(value))
		}
	}
	return indexes, blob, nil
}

func decodeSearchVectors(indexes []int32, blob []byte) ([]SearchVectorChunk, error) {
	if len(indexes) == 0 || len(blob) == 0 || len(blob)%(4*len(indexes)) != 0 {
		return nil, fmt.Errorf("shared search vectors hold %d bytes for %d chunks", len(blob), len(indexes))
	}
	dims := len(blob) / 4 / len(indexes)
	chunks := make([]SearchVectorChunk, len(indexes))
	for i, index := range indexes {
		values := make([]float32, dims)
		for j := range values {
			offset := (i*dims + j) * 4
			values[j] = math.Float32frombits(binary.LittleEndian.Uint32(blob[offset:]))
		}
		chunks[i] = SearchVectorChunk{Index: int(index), Vector: values}
	}
	return chunks, nil
}
