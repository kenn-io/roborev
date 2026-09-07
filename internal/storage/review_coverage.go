package storage

import "fmt"

// ReviewFileCoverage records how much of a review subject's change set the
// review input carried. A nil count means that count was not measured.
type ReviewFileCoverage struct {
	Reviewed *int `json:"reviewed,omitempty"`
	Excluded *int `json:"excluded,omitempty"`
}

// NormalizeReviewFileCoverage returns nil when no count was measured.
func NormalizeReviewFileCoverage(c *ReviewFileCoverage) *ReviewFileCoverage {
	if c == nil || (c.Reviewed == nil && c.Excluded == nil) {
		return nil
	}
	return c
}

// FormatSummary renders coverage for human-facing review details.
func (c *ReviewFileCoverage) FormatSummary() string {
	if c == nil {
		return ""
	}
	if c.Reviewed != nil && c.Excluded != nil {
		return formatCoverageCount(*c.Reviewed, "reviewed", true) +
			", " + formatCoverageCount(*c.Excluded, "excluded", false)
	}
	if c.Reviewed != nil {
		return formatCoverageCount(*c.Reviewed, "reviewed", true)
	}
	if c.Excluded != nil {
		return formatCoverageCount(*c.Excluded, "excluded", true)
	}
	return ""
}

func formatCoverageCount(count int, label string, includeNoun bool) string {
	if includeNoun {
		return fmt.Sprintf("%d file%s %s", count, pluralSuffix(count), label)
	}
	return fmt.Sprintf("%d %s", count, label)
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}
