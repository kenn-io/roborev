//go:build postgres

package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestVectorExchange(t *testing.T, pool *PgPool, machineID string) *VectorExchange {
	t.Helper()
	fingerprintCleanup(t, pool)
	return NewVectorExchange(func() *PgPool { return pool }, machineID)
}

// fingerprintCleanup removes every exchange row whose fingerprint starts with
// the test name, so tests sharing the database stay independent.
func fingerprintCleanup(t *testing.T, pool *PgPool) {
	t.Helper()
	cleanup := func() {
		ctx := context.Background()
		for _, table := range []string{"review_embeddings", "review_embedding_claims"} {
			_, _ = pool.pool.Exec(ctx, `DELETE FROM `+table+` WHERE generation_fingerprint LIKE $1`, t.Name()+"%")
		}
		_, _ = pool.pool.Exec(ctx, `DELETE FROM embedding_generations WHERE fingerprint LIKE $1`, t.Name()+"%")
	}
	cleanup()
	t.Cleanup(cleanup)
}

func TestIntegration_VectorExchangeSchemaIsUnversionedAndIdempotent(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	require.NoError(t, pool.EnsureSchema(ctx))
	require.NoError(t, pool.EnsureVectorExchangeSchema(ctx))
	present, err := pool.vectorExchangePresent(ctx)
	require.NoError(t, err)
	assert.True(t, present)

	var version int
	require.NoError(t, pool.pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version))
	assert.Equal(t, pgSchemaVersion, version, "exchange tables must not bump the sync schema version")
}

func TestIntegration_VectorExchangePublishLookupRoundTrip(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	exchange := newTestVectorExchange(t, pool, "machine-a")
	fingerprint := t.Name() + "-gen"

	target, err := exchange.Target(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, target)

	ok := VectorRecord{
		Key:    VectorKey{ReviewUUID: "00000000-0000-4000-8000-000000000001", ContentSHA256: "hash-1"},
		Status: VectorStatusOK, Dims: 2,
		Chunks: []VectorChunk{{Index: 0, Vector: []float32{0.6, 0.8}}, {Index: 2, Vector: []float32{1, 0}}},
	}
	skipped := VectorRecord{
		Key:    VectorKey{ReviewUUID: "00000000-0000-4000-8000-000000000002", ContentSHA256: "hash-2"},
		Status: VectorStatusSkipped, Dims: 2,
	}
	require.NoError(t, exchange.Publish(ctx, fingerprint, []VectorRecord{ok, skipped}))

	records, err := exchange.Lookup(ctx, fingerprint, []VectorKey{
		ok.Key, skipped.Key,
		{ReviewUUID: ok.Key.ReviewUUID, ContentSHA256: "other-text"},
	})
	require.NoError(t, err)
	byUUID := map[string]VectorRecord{}
	for _, record := range records {
		byUUID[record.Key.ReviewUUID] = record
	}
	require.Len(t, byUUID, 2, "a different content hash must not match")
	assert.Equal(t, ok.Chunks, byUUID[ok.Key.ReviewUUID].Chunks)
	assert.False(t, byUUID[ok.Key.ReviewUUID].Malformed)
	assert.Equal(t, VectorStatusSkipped, byUUID[skipped.Key.ReviewUUID].Status)
	assert.Empty(t, byUUID[skipped.Key.ReviewUUID].Chunks)

	other, err := exchange.Lookup(ctx, t.Name()+"-other-gen", []VectorKey{ok.Key})
	require.NoError(t, err)
	assert.Empty(t, other, "a different generation fingerprint must not match")
}

func TestIntegration_VectorExchangeTouchReportsRecreatedGeneration(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	exchange := newTestVectorExchange(t, pool, "machine-a")
	fingerprint := t.Name() + "-gen"
	gen := VectorGeneration{Fingerprint: fingerprint, Model: "m", Dimensions: 2}

	created, err := exchange.TouchGeneration(ctx, gen)
	require.NoError(t, err)
	assert.True(t, created)
	created, err = exchange.TouchGeneration(ctx, gen)
	require.NoError(t, err)
	assert.False(t, created)

	_, err = pool.pool.Exec(ctx, `DELETE FROM embedding_generations WHERE fingerprint = $1`, fingerprint)
	require.NoError(t, err)
	created, err = exchange.TouchGeneration(ctx, gen)
	require.NoError(t, err)
	assert.True(t, created)
}

func TestIntegration_VectorExchangeClaimIsExclusiveUntilExpiry(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	first := newTestVectorExchange(t, pool, "machine-a")
	second := NewVectorExchange(func() *PgPool { return pool }, "machine-b")
	fingerprint := t.Name() + "-gen"
	key := VectorKey{ReviewUUID: "00000000-0000-4000-8000-000000000003", ContentSHA256: "hash-3"}

	held, err := first.Claim(ctx, fingerprint, key, time.Minute)
	require.NoError(t, err)
	assert.True(t, held)
	held, err = second.Claim(ctx, fingerprint, key, time.Minute)
	require.NoError(t, err)
	assert.False(t, held, "a live claim by another machine blocks")
	held, err = first.Claim(ctx, fingerprint, key, time.Minute)
	require.NoError(t, err)
	assert.True(t, held, "the holder may renew")

	otherText := VectorKey{ReviewUUID: key.ReviewUUID, ContentSHA256: "hash-3b"}
	held, err = second.Claim(ctx, fingerprint, otherText, time.Minute)
	require.NoError(t, err)
	assert.True(t, held, "claims for different text never block each other")

	_, err = pool.pool.Exec(ctx, `UPDATE review_embedding_claims SET expires_at = NOW() - INTERVAL '1 second'
		WHERE generation_fingerprint = $1 AND content_sha256 = $2`, fingerprint, key.ContentSHA256)
	require.NoError(t, err)
	held, err = second.Claim(ctx, fingerprint, key, time.Minute)
	require.NoError(t, err)
	assert.True(t, held, "an expired claim can be taken over")

	require.NoError(t, second.Publish(ctx, fingerprint, []VectorRecord{{
		Key: key, Status: VectorStatusOK, Dims: 2,
		Chunks: []VectorChunk{{Index: 0, Vector: []float32{1, 0}}},
	}}))
	require.NoError(t, second.Release(ctx, fingerprint, []VectorKey{key}))
	held, err = first.Claim(ctx, fingerprint, key, time.Minute)
	require.NoError(t, err)
	assert.False(t, held, "nothing to claim once a record exists")

	require.NoError(t, second.Discard(ctx, fingerprint, key))
	held, err = first.Claim(ctx, fingerprint, key, time.Minute)
	require.NoError(t, err)
	assert.True(t, held, "a discarded record can be claimed again")
}

func TestIntegration_VectorExchangeGarbageCollectsUnusedGenerations(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	exchange := newTestVectorExchange(t, pool, "machine-a")
	stale := t.Name() + "-stale"
	live := t.Name() + "-live"
	record := VectorRecord{
		Key:    VectorKey{ReviewUUID: "00000000-0000-4000-8000-000000000004", ContentSHA256: "hash-4"},
		Status: VectorStatusOK, Dims: 2,
		Chunks: []VectorChunk{{Index: 0, Vector: []float32{1, 0}}},
	}
	for _, fingerprint := range []string{stale, live} {
		_, err := exchange.TouchGeneration(ctx, VectorGeneration{
			Fingerprint: fingerprint, Model: "m", Dimensions: 2, Params: map[string]string{"recipe": "2"},
		})
		require.NoError(t, err)
		require.NoError(t, exchange.Publish(ctx, fingerprint, []VectorRecord{record}))
	}
	_, err := pool.pool.Exec(ctx, `UPDATE embedding_generations SET last_used_at = NOW() - INTERVAL '31 days'
		WHERE fingerprint = $1`, stale)
	require.NoError(t, err)

	removed, err := exchange.CollectGarbage(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, removed, int64(1))

	staleRecords, err := exchange.Lookup(ctx, stale, []VectorKey{record.Key})
	require.NoError(t, err)
	assert.Empty(t, staleRecords)
	liveRecords, err := exchange.Lookup(ctx, live, []VectorKey{record.Key})
	require.NoError(t, err)
	assert.Len(t, liveRecords, 1)
}

func TestIntegration_VectorExchangeReportsUnsupportedAndUnreachable(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	disconnected := NewVectorExchange(func() *PgPool { return nil }, "machine-a")
	_, err := disconnected.Target(ctx)
	require.ErrorIs(t, err, ErrVectorExchangeUnreachable)

	_, err = pool.pool.Exec(ctx, `DROP TABLE review_embedding_claims`)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.EnsureVectorExchangeSchema(context.Background())) })
	exchange := NewVectorExchange(func() *PgPool { return pool }, "machine-a")
	_, err = exchange.Target(ctx)
	require.ErrorIs(t, err, ErrVectorExchangeUnsupported)
	_, err = exchange.Claim(ctx, t.Name(), VectorKey{ReviewUUID: "u", ContentSHA256: "h"}, time.Minute)
	require.ErrorIs(t, err, ErrVectorExchangeUnsupported)
}
