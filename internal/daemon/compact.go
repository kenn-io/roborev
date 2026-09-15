// ABOUTME: Compact job metadata handling for tracking source job IDs.
// ABOUTME: Used by worker to mark source jobs as closed when compact jobs complete.

package daemon

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/roborev/internal/config"
)

// CompactMetadata stores source job IDs for a compact job
type CompactMetadata struct {
	SourceJobIDs []int64 `json:"source_job_ids"`
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
