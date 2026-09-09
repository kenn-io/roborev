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
	Critical    int  `json:"critical"`
	High        int  `json:"high"`
	Medium      int  `json:"medium"`
	Low         int  `json:"low"`
	Approximate bool `json:"approximate"`
}

// ReviewFindingCounts classifies stored review output. A nonnil structured
// value is authoritative, including when it is malformed.
func ReviewFindingCounts(structuredOutput *string, proseOutput string) *FindingCounts {
	if structuredOutput != nil && *structuredOutput != "" {
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

	counts := &FindingCounts{Approximate: true}
	for _, label := range ProseSeverityLabels(strings.Split(proseOutput, "\n")) {
		if label.Legend {
			continue
		}
		switch label.Severity {
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
	if counts.Critical+counts.High+counts.Medium+counts.Low == 0 {
		return nil
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

func applyJobFindingCounts(job *ReviewJob, structuredOutput, proseOutput sql.NullString) {
	if !job.HasFindingCountsOutput() {
		return
	}
	var structured *string
	if structuredOutput.Valid && structuredOutput.String != "" {
		structured = &structuredOutput.String
	}
	job.FindingCounts = ReviewFindingCounts(structured, proseOutput.String)
}
