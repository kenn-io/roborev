package storage

import (
	"database/sql"
	"encoding/json"
	"strings"

	"go.kenn.io/roborev/internal/structuredreview"
)

// FindingCounts records every finding in a review, including findings below
// the review's minimum severity threshold.
type FindingCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

// ReviewFindingCounts counts severities in stored structured review output.
// Missing, invalid, or unable-to-review output has no counts.
func ReviewFindingCounts(structuredOutput *string) *FindingCounts {
	if structuredOutput == nil || *structuredOutput == "" {
		return nil
	}
	document, err := structuredreview.Decode(json.RawMessage(*structuredOutput))
	if err != nil || document.UnableToReview() {
		return nil
	}
	counts := &FindingCounts{}
	for _, finding := range document.Findings {
		switch strings.ToLower(finding.Severity) {
		case "critical":
			counts.Critical++
		case "high":
			counts.High++
		case "medium":
			counts.Medium++
		case "low":
			counts.Low++
		}
	}
	return counts
}

// HasFindingCountsOutput reports whether a job can have review finding data.
func (j ReviewJob) HasFindingCountsOutput() bool {
	if !j.HasViewableOutput() || j.Error != "" {
		return false
	}
	return j.IsReviewJob() || j.JobType == JobTypeCompact || j.IsSynthesisJob()
}

func applyJobFindingCounts(job *ReviewJob, structuredOutput sql.NullString) {
	if !job.HasFindingCountsOutput() {
		return
	}
	var structured *string
	if structuredOutput.Valid && structuredOutput.String != "" {
		structured = &structuredOutput.String
	}
	job.FindingCounts = ReviewFindingCounts(structured)
}
