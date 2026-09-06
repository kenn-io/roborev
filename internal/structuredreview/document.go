package structuredreview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

const SchemaVersion = 1

// Schema constrains a single reviewer's output.
var Schema = schema(false)

// SourcedSchema additionally requires every finding to cite the 1-based
// numbers of the input reviews that reported it. Synthesis uses it so a
// combined finding keeps its provenance.
var SourcedSchema = schema(true)

func schema(withSources bool) json.RawMessage {
	required := `["severity", "problem", "fix", "location"]`
	sources := ""
	if withSources {
		required = `["severity", "problem", "fix", "location", "sources"]`
		sources = `,
          "sources": {
            "type": "array",
            "minItems": 1,
            "items": {"type": "integer", "minimum": 1}
          }`
	}
	return json.RawMessage(fmt.Sprintf(`{
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
        "required": %s,
        "properties": {
          "severity": {
            "type": "string",
            "enum": ["critical", "high", "medium", "low"]
          },
          "problem": {"type": "string", "minLength": 1},
          "fix": {"type": "string", "minLength": 1},
          "location": {"type": ["string", "null"]}%s
        }
      }
    }
  }
}`, SchemaVersion, required, sources))
}

type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Summary       string    `json:"summary"`
	Findings      []Finding `json:"findings"`
	// SourceLabels names the input reviews a sourced document cites, indexed
	// by review number minus one. It is caller-provided, never decoded, and
	// only used when rendering Markdown.
	SourceLabels []string `json:"-"`
}

type Finding struct {
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
	Location string `json:"location,omitempty"`
	// Sources holds 1-based input review numbers for a sourced document.
	Sources []int `json:"sources,omitempty"`
}

// MarshalJSON emits an empty Location as JSON null so an encoded document
// stays valid under Schema, which requires the field on every finding.
func (f Finding) MarshalJSON() ([]byte, error) {
	type wire struct {
		Severity string  `json:"severity"`
		Problem  string  `json:"problem"`
		Fix      string  `json:"fix"`
		Location *string `json:"location"`
		Sources  []int   `json:"sources,omitempty"`
	}
	w := wire{Severity: f.Severity, Problem: f.Problem, Fix: f.Fix, Sources: f.Sources}
	if f.Location != "" {
		w.Location = &f.Location
	}
	return json.Marshal(w)
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
	Sources  []int           `json:"sources"`
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
			Sources:  finding.Sources,
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

// RequireSources validates a sourced document: every finding must cite at
// least one input review, and every citation must fall within the count of
// input reviews.
func (r Document) RequireSources(reviewCount int) error {
	for i, finding := range r.Findings {
		if len(finding.Sources) == 0 {
			return fmt.Errorf("structured review finding %d cites no source review", i+1)
		}
		for _, n := range finding.Sources {
			if n < 1 || n > reviewCount {
				return fmt.Errorf(
					"structured review finding %d cites review %d, but only %d reviews were provided",
					i+1, n, reviewCount,
				)
			}
		}
	}
	return nil
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
		SourceLabels:  r.SourceLabels,
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
		if labels := r.sourceLabels(finding); len(labels) > 0 {
			fmt.Fprintf(&out, "\n**Reported by:** %s\n", strings.Join(labels, ", "))
		}
	}
	return out.String()
}

// sourceLabels resolves a finding's review numbers to caller-provided labels,
// in citation order without duplicates. Numbers without a label are skipped.
func (r Document) sourceLabels(finding Finding) []string {
	labels := make([]string, 0, len(finding.Sources))
	for _, n := range finding.Sources {
		if n < 1 || n > len(r.SourceLabels) {
			continue
		}
		label := r.SourceLabels[n-1]
		if label == "" || slices.Contains(labels, label) {
			continue
		}
		labels = append(labels, label)
	}
	return labels
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
