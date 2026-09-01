package storage

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"
)

// ReleaseNote is one published GitHub release shown in the local clients.
type ReleaseNote struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Prerelease  bool      `json:"prerelease"`
}

// ReleaseNotesCache is the local copy of recent GitHub release notes.
type ReleaseNotesCache struct {
	ETag      string
	Releases  []ReleaseNote
	FetchedAt time.Time
}

func (db *DB) GetReleaseNotesCache(ctx context.Context) (*ReleaseNotesCache, error) {
	var etag, releasesJSON, fetchedAt string
	err := db.QueryRowContext(ctx, `
		SELECT etag, releases_json, fetched_at
		FROM release_notes_cache
		WHERE id = 1
	`).Scan(&etag, &releasesJSON, &fetchedAt)
	if err != nil {
		return nil, err
	}

	var releases []ReleaseNote
	if err := json.Unmarshal([]byte(releasesJSON), &releases); err != nil {
		return nil, fmt.Errorf("decode cached release notes: %w", err)
	}
	fetched, err := time.Parse(time.RFC3339Nano, fetchedAt)
	if err != nil {
		return nil, fmt.Errorf("parse release notes fetch time: %w", err)
	}
	return &ReleaseNotesCache{ETag: etag, Releases: releases, FetchedAt: fetched}, nil
}

func (db *DB) SaveReleaseNotesCache(ctx context.Context, cache ReleaseNotesCache) error {
	releasesJSON, err := json.Marshal(cache.Releases)
	if err != nil {
		return fmt.Errorf("encode release notes cache: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO release_notes_cache (id, etag, releases_json, fetched_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			etag = excluded.etag,
			releases_json = excluded.releases_json,
			fetched_at = excluded.fetched_at
	`, cache.ETag, string(releasesJSON), cache.FetchedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("save release notes cache: %w", err)
	}
	return nil
}

func (db *DB) TouchReleaseNotesCache(ctx context.Context, fetchedAt time.Time) error {
	_, err := db.ExecContext(ctx, `
		UPDATE release_notes_cache SET fetched_at = ? WHERE id = 1
	`, fetchedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("touch release notes cache: %w", err)
	}
	return nil
}
