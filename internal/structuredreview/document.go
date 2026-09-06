package structuredreview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

// SchemaVersion is the version agents must return. Version 1 documents
// (without a verdict) are still decoded so stored reviews keep working.
const SchemaVersion = 2

// Verdict values the agent may report. The verdict never relaxes a finding:
// a review with a finding at or above the severity threshold fails whatever
// the agent says. VerdictUnableToReview means the agent could not assess the
// change at all and is treated as an agent failure, not a clean pass.
const (
	VerdictPass           = "pass"
	VerdictFail           = "fail"
	VerdictUnableToReview = "unable_to_review"
)

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
  "required": ["schema_version", "summary", "verdict", "findings"],
  "properties": {
    "schema_version": {"type": "integer", "const": %d},
    "summary": {"type": "string", "minLength": 1},
    "verdict": {"type": "string", "enum": [%q, %q, %q]},
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
}`, SchemaVersion, VerdictPass, VerdictFail, VerdictUnableToReview, required, sources))
}

type Document struct {
	SchemaVersion int    `json:"schema_version"`
	Summary       string `json:"summary"`
	// Verdict is the agent's own judgment. Empty for version 1 documents.
	Verdict  string    `json:"verdict,omitempty"`
	Findings []Finding `json:"findings"`
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
	Verdict       string        `json:"verdict"`
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
		Verdict:       strings.ToLower(strings.TrimSpace(wire.Verdict)),
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
	switch result.SchemaVersion {
	case 1:
		if result.Verdict != "" {
			return Document{}, fmt.Errorf(
				"structured review schema_version 1 does not carry a verdict",
			)
		}
	case SchemaVersion:
		switch result.Verdict {
		case VerdictPass, VerdictFail, VerdictUnableToReview:
		default:
			return Document{}, fmt.Errorf(
				"structured review has invalid verdict %q", wire.Verdict,
			)
		}
	default:
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

// thresholdRank converts a minimum severity into the rank a finding must
// reach to count against the verdict. Empty and unknown values mean every
// finding counts, the same as "low".
func thresholdRank(minSeverity string) int {
	threshold := severityRank(strings.ToLower(strings.TrimSpace(minSeverity)))
	if threshold == 0 {
		threshold = severityRank("low")
	}
	return threshold
}

// UnableToReview reports whether the agent declared it could not assess the
// change. Callers treat that as an agent failure rather than a verdict.
func (r Document) UnableToReview() bool {
	return r.Verdict == VerdictUnableToReview
}

// Passed reports whether no finding reaches minSeverity. Findings below the
// threshold are kept in the document and rendered, but do not fail the review.
// The agent's own verdict is recorded and rendered but does not change the
// outcome; the findings are the source of truth.
func (r Document) Passed(minSeverity string) bool {
	threshold := thresholdRank(minSeverity)
	for _, finding := range r.Findings {
		if severityRank(finding.Severity) >= threshold {
			return false
		}
	}
	return true
}

// Markdown renders the summary and every finding. When minSeverity is set and
// no finding reaches it, the rendering says so before listing the findings so
// a reader can see why the review passed without losing the content.
func (r Document) Markdown(minSeverity string) string {
	var out strings.Builder
	out.WriteString("## Summary\n\n")
	out.WriteString(strings.TrimSpace(r.Summary))
	if r.Verdict != "" {
		fmt.Fprintf(&out, "\n\n**Agent assessment:** %s", verdictLabel(r.Verdict))
	}
	if len(r.Findings) == 0 {
		out.WriteString("\n\nNo issues found.\n")
		return out.String()
	}
	minSeverity = strings.ToLower(strings.TrimSpace(minSeverity))
	if severityRank(minSeverity) > severityRank("low") && r.Passed(minSeverity) {
		fmt.Fprintf(&out, "\n\nNo findings at or above %s severity.", minSeverity)
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

func verdictLabel(verdict string) string {
	switch verdict {
	case VerdictPass:
		return "Pass"
	case VerdictFail:
		return "Fail"
	case VerdictUnableToReview:
		return "Unable to review"
	default:
		return verdict
	}
}

func titleSeverity(severity string) string {
	if severity == "" {
		return "Finding"
	}
	return strings.ToUpper(severity[:1]) + severity[1:]
}
