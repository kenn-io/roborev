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

// validateCompactSources uses authoritative member rows, not listing metadata:
// a synthesis job's own review type does not identify the rubrics it combined.
func (s *Server) validateCompactSources(sourceIDs []int64) error {
	if len(sourceIDs) == 0 {
		return fmt.Errorf("compact source_job_ids are required")
	}
	for _, id := range sourceIDs {
		job, err := s.db.GetJobByID(id)
		if err != nil {
			return fmt.Errorf("load compact source %d: %w", id, err)
		}
		if job.IsSynthesisJob() {
			if job.PanelRunUUID == nil {
				return fmt.Errorf("compact source %d has no panel provenance", id)
			}
			members, err := s.db.GetPanelMemberReviews(*job.PanelRunUUID)
			if err != nil {
				return fmt.Errorf("load compact source %d members: %w", id, err)
			}
			if len(members) == 0 {
				return fmt.Errorf("compact source %d has no voting member provenance", id)
			}
			for _, member := range members {
				if !config.IsBuiltInReviewType(member.ReviewType) {
					return fmt.Errorf("compact source %d includes custom voting rubric %q; compact supports built-in reviews only", id, member.ReviewType)
				}
			}
		} else if !config.IsBuiltInReviewType(job.ReviewType) {
			return fmt.Errorf("compact source %d uses custom rubric %q; compact supports built-in reviews only", id, job.ReviewType)
		}
	}
	return nil
}
