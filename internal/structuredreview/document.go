package structuredreview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const SchemaVersion = 1

var Schema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "summary", "findings"],
  "properties": {
    "schema_version": {"type": "integer", "const": %d},
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
}`, SchemaVersion))

type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Summary       string    `json:"summary"`
	Findings      []Finding `json:"findings"`
}

type Finding struct {
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
	Location string `json:"location,omitempty"`
}

type documentWire struct {
	SchemaVersion int           `json:"schema_version"`
	Summary       string        `json:"summary"`
	Findings      []findingWire `json:"findings"`
}

type findingWire struct {
	Severity string          `json:"severity"`
	Problem  string          `json:"problem"`
	Fix      string          `json:"fix"`
	Location json.RawMessage `json:"location"`
}

func Decode(raw json.RawMessage) (Document, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire documentWire
	if err := dec.Decode(&wire); err != nil {
		return Document{}, fmt.Errorf("decode structured review: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Document{}, err
	}
	if wire.Findings == nil {
		return Document{}, fmt.Errorf("structured review findings is required")
	}
	result := Document{
		SchemaVersion: wire.SchemaVersion,
		Summary:       wire.Summary,
		Findings:      make([]Finding, len(wire.Findings)),
	}
	for i, finding := range wire.Findings {
		if len(finding.Location) == 0 {
			return Document{}, fmt.Errorf(
				"structured review finding %d has no location field", i+1,
			)
		}
		var location string
		if !bytes.Equal(finding.Location, []byte("null")) {
			if err := json.Unmarshal(finding.Location, &location); err != nil {
				return Document{}, fmt.Errorf(
					"structured review finding %d has invalid location: %w", i+1, err,
				)
			}
		}
		result.Findings[i] = Finding{
			Severity: finding.Severity,
			Problem:  finding.Problem,
			Fix:      finding.Fix,
			Location: location,
		}
	}
	if result.SchemaVersion != SchemaVersion {
		return Document{}, fmt.Errorf(
			"unsupported structured review schema_version %d",
			result.SchemaVersion,
		)
	}
	result.Summary = strings.TrimSpace(result.Summary)
	if result.Summary == "" {
		return Document{}, fmt.Errorf("structured review summary is required")
	}
	for i := range result.Findings {
		finding := &result.Findings[i]
		finding.Severity = strings.ToLower(strings.TrimSpace(finding.Severity))
		finding.Problem = strings.TrimSpace(finding.Problem)
		finding.Fix = strings.TrimSpace(finding.Fix)
		finding.Location = strings.TrimSpace(finding.Location)
		if severityRank(finding.Severity) == 0 {
			return Document{}, fmt.Errorf(
				"structured review finding %d has invalid severity %q",
				i+1, finding.Severity,
			)
		}
		if finding.Problem == "" {
			return Document{}, fmt.Errorf(
				"structured review finding %d has no problem", i+1,
			)
		}
		if finding.Fix == "" {
			return Document{}, fmt.Errorf(
				"structured review finding %d has no fix", i+1,
			)
		}
	}
	return result, nil
}

func ensureEOF(dec *json.Decoder) error {
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

func (r Document) Filter(minSeverity string) Document {
	threshold := severityRank(strings.ToLower(strings.TrimSpace(minSeverity)))
	if threshold == 0 {
		threshold = severityRank("low")
	}
	filtered := Document{
		SchemaVersion: r.SchemaVersion,
		Summary:       r.Summary,
		Findings:      make([]Finding, 0, len(r.Findings)),
	}
	for _, finding := range r.Findings {
		if severityRank(finding.Severity) >= threshold {
			filtered.Findings = append(filtered.Findings, finding)
		}
	}
	return filtered
}

func (r Document) Passed() bool {
	return len(r.Findings) == 0
}

func (r Document) Markdown() string {
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
