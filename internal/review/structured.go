package review

import (
	"encoding/json"
	"strings"

	"go.kenn.io/roborev/internal/structuredreview"
)

var CustomReviewSchema = structuredreview.Schema

type StructuredReview = structuredreview.Document

type StructuredFinding = structuredreview.Finding

func DecodeStructuredReview(raw json.RawMessage) (StructuredReview, error) {
	return structuredreview.Decode(raw)
}

func stricterMinSeverity(first, second string) string {
	first = strings.ToLower(strings.TrimSpace(first))
	second = strings.ToLower(strings.TrimSpace(second))
	if severityRank(first) >= severityRank(second) {
		return first
	}
	return second
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}
