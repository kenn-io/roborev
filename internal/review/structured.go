package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

var CustomReviewSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "findings"],
  "properties": {
    "summary": {"type": "string", "minLength": 1},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["severity", "problem", "fix", "location"],
        "properties": {
          "severity": {
            "type": "string",
            "enum": ["critical", "high", "medium", "low"]
          },
          "problem": {"type": "string", "minLength": 1},
          "fix": {"type": "string", "minLength": 1},
          "location": {"type": ["string", "null"]}
        }
      }
    }
  }
}`)

type StructuredReview struct {
	Summary  string              `json:"summary"`
	Findings []StructuredFinding `json:"findings"`
}

type StructuredFinding struct {
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
	Location string `json:"location,omitempty"`
}

func DecodeStructuredReview(raw json.RawMessage) (StructuredReview, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var result StructuredReview
	if err := dec.Decode(&result); err != nil {
		return StructuredReview{}, fmt.Errorf("decode structured review: %w", err)
	}
	if err := ensureStructuredReviewEOF(dec); err != nil {
		return StructuredReview{}, err
	}
	result.Summary = strings.TrimSpace(result.Summary)
	if result.Summary == "" {
		return StructuredReview{}, fmt.Errorf("structured review summary is required")
	}
	if result.Findings == nil {
		return StructuredReview{}, fmt.Errorf("structured review findings is required")
	}
	for i := range result.Findings {
		finding := &result.Findings[i]
		finding.Severity = strings.ToLower(strings.TrimSpace(finding.Severity))
		finding.Problem = strings.TrimSpace(finding.Problem)
		finding.Fix = strings.TrimSpace(finding.Fix)
		finding.Location = strings.TrimSpace(finding.Location)
		if severityRank(finding.Severity) == 0 {
			return StructuredReview{}, fmt.Errorf(
				"structured review finding %d has invalid severity %q",
				i+1, finding.Severity,
			)
		}
		if finding.Problem == "" {
			return StructuredReview{}, fmt.Errorf(
				"structured review finding %d has no problem", i+1,
			)
		}
		if finding.Fix == "" {
			return StructuredReview{}, fmt.Errorf(
				"structured review finding %d has no fix", i+1,
			)
		}
	}
	return result, nil
}

func ensureStructuredReviewEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing structured review data: %w", err)
	}
	return fmt.Errorf("structured review contains trailing JSON")
}

func (r StructuredReview) Filter(minSeverity string) StructuredReview {
	threshold := severityRank(strings.ToLower(strings.TrimSpace(minSeverity)))
	if threshold == 0 {
		threshold = severityRank("low")
	}
	filtered := StructuredReview{
		Summary:  r.Summary,
		Findings: make([]StructuredFinding, 0, len(r.Findings)),
	}
	for _, finding := range r.Findings {
		if severityRank(finding.Severity) >= threshold {
			filtered.Findings = append(filtered.Findings, finding)
		}
	}
	return filtered
}

func (r StructuredReview) Passed() bool {
	return len(r.Findings) == 0
}

func (r StructuredReview) Markdown() string {
	var out strings.Builder
	out.WriteString("## Summary\n\n")
	out.WriteString(strings.TrimSpace(r.Summary))
	if len(r.Findings) == 0 {
		out.WriteString("\n\nNo findings at or above the configured severity threshold.\n")
		return out.String()
	}
	out.WriteString("\n\n## Findings\n")
	for i, finding := range r.Findings {
		fmt.Fprintf(&out, "\n### %d. %s\n\n", i+1, titleSeverity(finding.Severity))
		if finding.Location != "" {
			fmt.Fprintf(&out, "**Location:** %s\n\n", finding.Location)
		}
		fmt.Fprintf(&out, "**Problem:** %s\n\n", finding.Problem)
		fmt.Fprintf(&out, "**Fix:** %s\n", finding.Fix)
	}
	return out.String()
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

func titleSeverity(severity string) string {
	if severity == "" {
		return "Finding"
	}
	return strings.ToUpper(severity[:1]) + severity[1:]
}
