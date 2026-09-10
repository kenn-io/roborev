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
	for _, label := range reviewProseSeverityLabels(strings.Split(proseOutput, "\n")) {
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

func reviewProseSeverityLabels(lines []string) []ProseLabel {
	labels := ProseSeverityLabels(lines)
	headingSeverity := ""
	headingLevel := 0
	fieldSeen := false

	for i, line := range lines {
		level := proseMarkdownHeadingLevel(line)
		if severity := proseSeverityHeading(line); severity != "" {
			labels[i] = ProseLabel{Severity: severity}
			headingSeverity = severity
			headingLevel = level
			fieldSeen = false
			continue
		}
		if level > 0 && headingSeverity != "" && level <= headingLevel {
			headingSeverity = ""
			headingLevel = 0
			fieldSeen = false
		}
		if headingSeverity == "" || labels[i].Severity != headingSeverity ||
			!proseSeverityField(line) || fieldSeen {
			continue
		}
		labels[i].Severity = ""
		fieldSeen = true
	}
	return labels
}

func proseSeverityHeading(line string) string {
	if proseMarkdownHeadingLevel(line) == 0 {
		return ""
	}
	heading := strings.ToLower(stripMarkdown(strings.TrimSpace(line)))
	for _, severity := range []string{"critical", "high", "medium", "low"} {
		if heading == severity+" severity" || strings.HasPrefix(heading, severity+" severity ") {
			return severity
		}
	}
	return ""
}

func proseMarkdownHeadingLevel(line string) int {
	trimmed := strings.TrimSpace(line)
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || (level < len(trimmed) && trimmed[level] != ' ') {
		return 0
	}
	return level
}

func proseSeverityField(line string) bool {
	field := strings.ToLower(stripMarkdown(stripListMarker(strings.TrimSpace(line))))
	return strings.HasPrefix(field, "severity:") || strings.HasPrefix(field, "severity|") ||
		strings.HasPrefix(field, "severity -") || strings.HasPrefix(field, "severity —") ||
		strings.HasPrefix(field, "severity –")
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
