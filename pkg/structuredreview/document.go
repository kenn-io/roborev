// Package structuredreview defines the canonical JSON document that roborev
// stores for a review, and the code that reads and renders it.
//
// This package is a supported import for other Go modules:
//
//	import "go.kenn.io/roborev/pkg/structuredreview"
//
// It imports only the standard library. A consumer that receives a document,
// for example the "document" field of `roborev export reviews`, can decode and
// render it with the same code roborev uses instead of copying it.
//
//   - [Document] is one review: a summary, the agent's verdict, and a list of
//     [Finding] values. Encode it with the standard JSON encoder to get the
//     canonical form. An empty finding location encodes as JSON null.
//   - [Decode] parses and validates a JSON document. It rejects unknown
//     fields, trailing data, unsupported versions, and findings with an
//     invalid severity or kind, or without a problem or fix. It trims text and
//     lowercases the severity, kind, and verdict, so a decoded Document is
//     normalized.
//   - [Document.Markdown] renders a Document as the Markdown that roborev
//     shows and exports as review content.
//
// [SchemaVersion] versions the document, not this Go API. It is the version
// agents must return and the value of "schema_version" in new review
// documents. Version 3 findings carry a kind, a confidence, and a file and
// line range. Versions 1 and 2 carry a free-text location instead, and
// version 1 has no verdict; Decode accepts both so stored reviews keep
// working. [Schema] and [SourcedSchema] are the JSON Schemas that constrain
// agent output to the current version. Storage-only version 0 carries original
// historical Markdown in Legacy, without extracted findings.
package structuredreview

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// SchemaVersion is the version agents must return. Versions 1 and 2 are still
// decoded so stored reviews keep working.
const SchemaVersion = 3

// TextLocationSchemaVersion is the last version whose findings carry a
// free-text location instead of a kind, confidence, file, and line range.
// Producers that only have free-text locations, such as goal reviews and
// converted historical Markdown, write this version.
const TextLocationSchemaVersion = 2

// Kind values tell a proven fault apart from a suggestion. A version 3
// finding always has one.
const (
	// KindDefect means the code behaves wrongly in a case the finding names.
	KindDefect = "defect"
	// KindImprovement means the code works, but something should still
	// change: a missing or weak test, wrong documentation, or structure,
	// duplication, or naming that will cost later work.
	KindImprovement = "improvement"
)

// Kinds lists every kind in the order the schema offers them.
var Kinds = []string{KindDefect, KindImprovement}

// Topic values name the area a finding is about, independent of its kind:
// a defect can be a security problem, and an improvement can be a missing
// test. A version 3 finding has at most one topic, and may have none.
const (
	TopicTests         = "tests"
	TopicDocumentation = "documentation"
	TopicSecurity      = "security"
	TopicPerformance   = "performance"
	TopicDesign        = "design"
)

// Topics lists every topic in the order the schema offers them.
var Topics = []string{TopicTests, TopicDocumentation, TopicSecurity, TopicPerformance, TopicDesign}

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

func schema(withSources bool) jsontext.Value {
	required := `["severity", "kind", "topic", "confidence", "problem", "fix", "file", "start_line", "end_line"]`
	sources := ""
	if withSources {
		required = `["severity", "kind", "topic", "confidence", "problem", "fix", "file", "start_line", "end_line", "sources"]`
		sources = `,
          "sources": {
            "type": "array",
            "minItems": 1,
            "items": {"type": "integer", "minimum": 1}
          }`
	}
	return jsontext.Value(fmt.Sprintf(`{
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
          "kind": {
            "type": "string",
            "enum": [%q, %q]
          },
          "topic": {
            "type": ["string", "null"],
            "enum": [%q, %q, %q, %q, %q, null]
          },
          "confidence": {"type": "number", "minimum": 0, "maximum": 1},
          "problem": {"type": "string", "minLength": 1},
          "fix": {"type": "string", "minLength": 1},
          "file": {"type": ["string", "null"]},
          "start_line": {"type": ["integer", "null"], "minimum": 1},
          "end_line": {"type": ["integer", "null"], "minimum": 1}%s
        }
      }
    }
  }
}`, SchemaVersion, VerdictPass, VerdictFail, VerdictUnableToReview, required,
		KindDefect, KindImprovement,
		TopicTests, TopicDocumentation, TopicSecurity, TopicPerformance, TopicDesign, sources))
}

// LegacyDocument preserves historical output without claiming extracted findings.
// RecordedVerdict is nil when the historical outcome is unknown.
type LegacyDocument struct {
	Markdown        string `json:"markdown"`
	RecordedVerdict *bool  `json:"recorded_verdict"`
}

// LegacySchemaVersion is storage-only; agents must return SchemaVersion.
const LegacySchemaVersion = 0

type Document struct {
	Legacy        *LegacyDocument `json:"legacy,omitempty"`
	SchemaVersion int             `json:"schema_version"`
	Summary       string          `json:"summary"`
	// Verdict is the agent's own judgment. Empty for version 1 documents.
	Verdict  string    `json:"verdict,omitempty"`
	Findings []Finding `json:"findings"`
	// SourceLabels names the input reviews a sourced document cites, indexed
	// by review number minus one. Persist these labels with synthesized
	// documents so source attribution survives storage and sync.
	SourceLabels []string `json:"source_labels,omitempty"`
}

// Finding is one problem a review reports.
//
// A version 3 finding has a Kind, an optional Topic, a Confidence, and a File
// with an optional line range; its Location is derived from them by [FormatLocation]. Older
// findings have an empty Kind and only a free-text Location. A non-empty Kind
// is what marks a finding as version 3 when it is encoded.
type Finding struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind,omitempty"`
	// Topic is one of [Topics], or empty when no topic applies.
	Topic string `json:"topic,omitempty"`
	// Confidence is the reviewer's estimate, from 0 to 1, that the problem is
	// real. Models are poorly calibrated, so treat it as a weak signal.
	Confidence float64 `json:"confidence,omitempty"`
	Problem    string  `json:"problem"`
	Fix        string  `json:"fix"`
	// File is a path relative to the repository root. StartLine and EndLine
	// are 1-based and inclusive, or zero when the finding covers the whole
	// file or has no file.
	File      string `json:"file,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	// Location is a display form of where the problem is, or empty.
	Location string `json:"location,omitempty"`
	// Sources holds 1-based input review numbers for a sourced document.
	Sources []int `json:"sources,omitempty"`
}

// FormatLocation renders a file and line range as "path", "path:12", or
// "path:12-20". It returns "" when file is empty.
func FormatLocation(file string, startLine, endLine int) string {
	switch {
	case file == "":
		return ""
	case startLine <= 0:
		return file
	case endLine <= startLine:
		return fmt.Sprintf("%s:%d", file, startLine)
	default:
		return fmt.Sprintf("%s:%d-%d", file, startLine, endLine)
	}
}

// SetLocation sets a version 3 finding's file and line range and the
// Location derived from them. A zero startLine clears the range; an endLine
// before startLine becomes startLine.
func (f *Finding) SetLocation(file string, startLine, endLine int) {
	if file == "" || startLine <= 0 {
		startLine, endLine = 0, 0
	} else if endLine < startLine {
		endLine = startLine
	}
	f.File, f.StartLine, f.EndLine = file, startLine, endLine
	f.Location = FormatLocation(file, startLine, endLine)
}

// MarshalJSON writes the shape of the finding's version. Every finding
// carries a location, as JSON null when empty, so readers of older documents
// keep working. A version 3 finding also carries its kind, topic, confidence,
// file, and line range, with null for an absent topic, file, or range.
func (f Finding) MarshalJSON() ([]byte, error) {
	var location *string
	if f.Location != "" {
		location = &f.Location
	}
	if f.Kind == "" {
		type wire struct {
			Severity string  `json:"severity"`
			Problem  string  `json:"problem"`
			Fix      string  `json:"fix"`
			Location *string `json:"location"`
			Sources  []int   `json:"sources,omitempty"`
		}
		return json.Marshal(wire{Severity: f.Severity, Problem: f.Problem, Fix: f.Fix, Location: location, Sources: f.Sources})
	}
	type wire struct {
		Severity   string  `json:"severity"`
		Kind       string  `json:"kind"`
		Topic      *string `json:"topic"`
		Confidence float64 `json:"confidence"`
		Problem    string  `json:"problem"`
		Fix        string  `json:"fix"`
		File       *string `json:"file"`
		StartLine  *int    `json:"start_line"`
		EndLine    *int    `json:"end_line"`
		Location   *string `json:"location"`
		Sources    []int   `json:"sources,omitempty"`
	}
	w := wire{
		Severity: f.Severity, Kind: f.Kind, Confidence: f.Confidence,
		Problem: f.Problem, Fix: f.Fix, Location: location, Sources: f.Sources,
	}
	if f.Topic != "" {
		w.Topic = &f.Topic
	}
	if f.File != "" {
		w.File = &f.File
		if f.StartLine > 0 {
			w.StartLine, w.EndLine = &f.StartLine, &f.EndLine
		}
	}
	return json.Marshal(w)
}

type documentWire struct {
	Legacy        *LegacyDocument `json:"legacy,omitempty"`
	SchemaVersion int             `json:"schema_version"`
	Summary       string          `json:"summary"`
	Verdict       string          `json:"verdict"`
	Findings      []findingWire   `json:"findings"`
	SourceLabels  []string        `json:"source_labels,omitempty"`
}

type findingWire struct {
	Severity   string         `json:"severity"`
	Kind       *string        `json:"kind"`
	Topic      *string        `json:"topic"`
	Confidence *float64       `json:"confidence"`
	Problem    string         `json:"problem"`
	Fix        string         `json:"fix"`
	File       *string        `json:"file"`
	StartLine  *int           `json:"start_line"`
	EndLine    *int           `json:"end_line"`
	Location   jsontext.Value `json:"location"`
	Sources    []int          `json:"sources"`
}

func Decode(raw jsontext.Value) (Document, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw))
	var wire documentWire
	if err := json.UnmarshalDecode(dec, &wire, json.RejectUnknownMembers(true)); err != nil {
		return Document{}, fmt.Errorf("decode structured review: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Document{}, err
	}
	if wire.Legacy != nil {
		if wire.SchemaVersion != LegacySchemaVersion || wire.Summary != "" || wire.Verdict != "" || len(wire.Findings) != 0 || len(wire.SourceLabels) != 0 {
			return Document{}, fmt.Errorf("legacy document cannot contain structured review fields")
		}
		return Document{SchemaVersion: LegacySchemaVersion, Legacy: wire.Legacy}, nil
	}
	if wire.Findings == nil {
		return Document{}, fmt.Errorf("structured review findings is required")
	}
	result := Document{
		SchemaVersion: wire.SchemaVersion,
		SourceLabels:  wire.SourceLabels,
		Summary:       wire.Summary,
		Verdict:       strings.ToLower(strings.TrimSpace(wire.Verdict)),
		Findings:      make([]Finding, len(wire.Findings)),
	}
	for i, finding := range wire.Findings {
		var err error
		if wire.SchemaVersion >= 3 {
			result.Findings[i], err = decodeLocatedFinding(finding)
		} else {
			result.Findings[i], err = decodeTextLocatedFinding(finding)
		}
		if err != nil {
			return Document{}, fmt.Errorf("structured review finding %d %w", i+1, err)
		}
	}
	switch result.SchemaVersion {
	case 1:
		if result.Verdict != "" {
			return Document{}, fmt.Errorf(
				"structured review schema_version 1 does not carry a verdict",
			)
		}
	case TextLocationSchemaVersion, SchemaVersion:
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

// decodeTextLocatedFinding reads a version 1 or 2 finding, which must carry a
// location field and none of the version 3 fields.
func decodeTextLocatedFinding(wire findingWire) (Finding, error) {
	if wire.Kind != nil || wire.Topic != nil || wire.Confidence != nil || wire.File != nil || wire.StartLine != nil || wire.EndLine != nil {
		return Finding{}, fmt.Errorf("has kind, topic, confidence, file, or line fields, which need schema_version 3")
	}
	if len(wire.Location) == 0 {
		return Finding{}, fmt.Errorf("has no location field")
	}
	var location string
	if !bytes.Equal(wire.Location, []byte("null")) {
		if err := json.Unmarshal(wire.Location, &location); err != nil {
			return Finding{}, fmt.Errorf("has invalid location: %w", err)
		}
	}
	return Finding{
		Severity: wire.Severity,
		Problem:  wire.Problem,
		Fix:      wire.Fix,
		Location: strings.TrimSpace(location),
		Sources:  wire.Sources,
	}, nil
}

// decodeLocatedFinding reads a version 3 finding. Kind and confidence are
// required. A missing topic, file, or line is read as null, and a stored location is
// ignored because it is derived from the file and lines.
func decodeLocatedFinding(wire findingWire) (Finding, error) {
	if wire.Kind == nil {
		return Finding{}, fmt.Errorf("has no kind")
	}
	kind := strings.ToLower(strings.TrimSpace(*wire.Kind))
	if !slices.Contains(Kinds, kind) {
		return Finding{}, fmt.Errorf("has invalid kind %q", *wire.Kind)
	}
	var topic string
	if wire.Topic != nil {
		topic = strings.ToLower(strings.TrimSpace(*wire.Topic))
		if topic != "" && !slices.Contains(Topics, topic) {
			return Finding{}, fmt.Errorf("has invalid topic %q", *wire.Topic)
		}
	}
	if wire.Confidence == nil {
		return Finding{}, fmt.Errorf("has no confidence")
	}
	if *wire.Confidence < 0 || *wire.Confidence > 1 {
		return Finding{}, fmt.Errorf("has confidence %v outside 0 to 1", *wire.Confidence)
	}
	if len(wire.Location) > 0 && !bytes.Equal(wire.Location, []byte("null")) {
		var location string
		if err := json.Unmarshal(wire.Location, &location); err != nil {
			return Finding{}, fmt.Errorf("has invalid location: %w", err)
		}
	}
	finding := Finding{
		Severity:   wire.Severity,
		Kind:       kind,
		Topic:      topic,
		Confidence: *wire.Confidence,
		Problem:    wire.Problem,
		Fix:        wire.Fix,
		Sources:    wire.Sources,
	}
	var file string
	if wire.File != nil {
		file = strings.TrimSpace(*wire.File)
	}
	start, end := 0, 0
	if wire.StartLine != nil {
		start = *wire.StartLine
	}
	if wire.EndLine != nil {
		end = *wire.EndLine
	}
	if start < 0 || end < 0 {
		return Finding{}, fmt.Errorf("has a negative line number")
	}
	if start == 0 {
		// An end line alone still names a line.
		start = end
	}
	finding.SetLocation(file, start, end)
	return finding, nil
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

func ensureEOF(dec *jsontext.Decoder) error {
	var extra any
	err := json.UnmarshalDecode(dec, &extra, json.RejectUnknownMembers(true))
	if errors.Is(err, io.EOF) {
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
// outcome; the findings are the source of truth. Legacy documents return their
// recorded outcome; an unknown historical outcome returns false.
func (r Document) Passed(minSeverity string) bool {
	if r.Legacy != nil {
		return r.Legacy.RecordedVerdict != nil && *r.Legacy.RecordedVerdict
	}
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
	if r.Legacy != nil {
		return "**Unstructured historical review.** Finding counts are unavailable.\n\n" + r.Legacy.Markdown
	}
	var out strings.Builder
	out.WriteString("## Summary\n\n")
	out.WriteString(strings.TrimSpace(r.Summary))
	if r.Verdict != "" {
		fmt.Fprintf(&out, "\n\n**Agent assessment:** %s", verdictLabel(r.Verdict))
	}
	if r.UnableToReview() {
		return out.String() + "\n"
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
		// Kind, topic, and confidence are data only. Rendered Markdown is
		// posted to pull requests and never labels a finding with them.
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
