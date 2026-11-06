// ABOUTME: Compact job metadata handling for tracking source job IDs.
// ABOUTME: Used by worker to mark source jobs as closed when compact jobs complete.

package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/review"
)

// CompactMetadata stores source job IDs for a compact job
type CompactMetadata struct {
	SourceJobIDs []int64                 `json:"source_job_ids"`
	Manifest     *review.CompactManifest `json:"manifest,omitempty"`
}

// ReadCompactManifest waits using the existing metadata retry policy. The CLI
// can only write metadata after enqueue assigns a job ID. Never fall back to
// ordinary review output when a marked job's snapshot is unavailable.
func ReadCompactManifest(ctx context.Context, jobID int64, storedPrompt string) (*review.CompactManifest, error) {
	return backoff.Retry(ctx, func() (*review.CompactManifest, error) {
		metadata, err := ReadCompactMetadata(jobID)
		if err != nil {
			return nil, err
		}
		if metadata.Manifest == nil {
			return nil, fmt.Errorf("compact manifest missing")
		}
		if err := review.ValidateCompactManifest(storedPrompt, *metadata.Manifest); err != nil {
			return nil, err
		}
		return metadata.Manifest, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(500*time.Millisecond)),
		backoff.WithMaxTries(3), backoff.WithMaxElapsedTime(0))
}

// WriteCompactMetadata publishes a complete snapshot with one atomic rename.
func WriteCompactMetadata(jobID int64, metadata CompactMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	path := compactMetadataPath(jobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".compact-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return atomicfile.Replace(file.Name(), path)
}

// ReadCompactMetadata retrieves source job IDs for a compact job
func ReadCompactMetadata(jobID int64) (*CompactMetadata, error) {
	path := compactMetadataPath(jobID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read metadata file: %w", err)
	}

	var metadata CompactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("parse metadata JSON: %w", err)
	}

	return &metadata, nil
}

// DeleteCompactMetadata removes the metadata file after processing
func DeleteCompactMetadata(jobID int64) error {
	path := compactMetadataPath(jobID)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete metadata file: %w", err)
	}
	return nil
}

// compactMetadataPath returns the file path for compact job metadata
func compactMetadataPath(jobID int64) string {
	return filepath.Join(config.DataDir(), fmt.Sprintf("compact-%d.json", jobID))
}
