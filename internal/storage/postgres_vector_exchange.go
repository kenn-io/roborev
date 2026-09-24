package storage

import (
	"context"
	"fmt"
	"log"
)

// vectorExchangeDDL creates the shared review-vector tables. The DDL is
// unversioned and idempotent on purpose: bumping pgSchemaVersion would make
// every older daemon refuse the database, while unknown extra tables are
// ignored by them.
var vectorExchangeDDL = []string{
	`CREATE TABLE IF NOT EXISTS embedding_generations (
		fingerprint TEXT PRIMARY KEY,
		descriptor JSONB NOT NULL,
		last_machine_id TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS review_embeddings (
		review_uuid TEXT NOT NULL,
		generation_fingerprint TEXT NOT NULL,
		content_sha256 TEXT NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('ok', 'skipped')),
		dims INTEGER NOT NULL,
		chunk_indexes INTEGER[] NOT NULL,
		chunks BYTEA[] NOT NULL,
		publisher_machine_id TEXT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		superseded_at TIMESTAMPTZ,
		PRIMARY KEY (review_uuid, generation_fingerprint, content_sha256)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_review_embeddings_generation
		ON review_embeddings(generation_fingerprint)`,
	`CREATE TABLE IF NOT EXISTS review_embedding_claims (
		review_uuid TEXT NOT NULL,
		generation_fingerprint TEXT NOT NULL,
		content_sha256 TEXT NOT NULL,
		machine_id TEXT NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (review_uuid, generation_fingerprint, content_sha256)
	)`,
}

var vectorExchangeUpgradeDDL = []string{
	`ALTER TABLE review_embeddings ADD COLUMN IF NOT EXISTS superseded_at TIMESTAMPTZ`,
	`CREATE INDEX IF NOT EXISTS idx_review_embeddings_superseded_at
		ON review_embeddings(superseded_at) WHERE superseded_at IS NOT NULL`,
}

// EnsureVectorExchangeSchema creates the shared review-vector tables when any
// is missing. A role without CREATE privilege gets an error; callers treat
// that as "exchange unsupported" and keep syncing.
func (p *PgPool) EnsureVectorExchangeSchema(ctx context.Context) error {
	present, err := p.vectorExchangePresent(ctx)
	if err != nil {
		return fmt.Errorf("inspect vector exchange schema: %w", err)
	}
	if !present {
		for _, stmt := range vectorExchangeDDL {
			if _, err := p.pool.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("create vector exchange schema: %w", err)
			}
		}
	}
	for _, stmt := range vectorExchangeUpgradeDDL {
		if _, err := p.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("update vector exchange schema: %w", err)
		}
	}
	return nil
}

func (p *PgPool) vectorExchangePresent(ctx context.Context) (bool, error) {
	var present bool
	err := p.pool.QueryRow(ctx, `
		SELECT to_regclass('embedding_generations') IS NOT NULL
		   AND to_regclass('review_embeddings') IS NOT NULL
		   AND to_regclass('review_embedding_claims') IS NOT NULL
		   AND EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema = current_schema()
			   AND table_name = 'review_embeddings'
			   AND column_name = 'superseded_at')`).Scan(&present)
	return present, err
}

func (p *PgPool) ensureVectorExchangeSchemaBestEffort(ctx context.Context) {
	if err := p.EnsureVectorExchangeSchema(ctx); err != nil {
		log.Printf("Sync: shared search vectors unavailable: %v", err)
	}
}
