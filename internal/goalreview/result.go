package goalreview

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/roborev/pkg/structuredreview"
)

type Location struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	KataID string `json:"kata_id,omitempty"`
}

type Finding struct {
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Fix      string   `json:"fix"`
	Location Location `json:"location"`
}

// DecodeStrict also rejects duplicate keys, which encoding/json otherwise
// accepts with last-value-wins semantics.
func DecodeStrict(raw string, value any) error {
	tokens := json.NewDecoder(strings.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := tokens.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]bool{}
			for tokens.More() {
				key, err := tokens.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("invalid JSON key")
				}
				if name != strings.ToLower(name) {
					return fmt.Errorf("JSON field must match its schema name exactly: %q", name)
				}
				if keys[name] {
					return fmt.Errorf("duplicate JSON key %s", name)
				}
				keys[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for tokens.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
		_, err = tokens.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := tokens.Token(); err != io.EOF {
		return fmt.Errorf("extra JSON content")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func ParseResult(raw string, snapshot Snapshot) ([]Finding, error) {
	var result struct {
		Findings *[]Finding `json:"findings"`
	}
	if err := DecodeStrict(raw, &result); err != nil {
		return nil, fmt.Errorf("invalid goal review result: %w", err)
	}
	if result.Findings == nil {
		return nil, fmt.Errorf("goal review result requires a findings array")
	}
	findings := *result.Findings
	for i := range findings {
		finding := &findings[i]
		if finding.Severity != "high" && finding.Severity != "medium" && finding.Severity != "low" {
			return nil, fmt.Errorf("invalid finding severity")
		}
		if strings.TrimSpace(finding.Message) == "" {
			return nil, fmt.Errorf("empty finding message")
		}
		if strings.TrimSpace(finding.Fix) == "" {
			return nil, fmt.Errorf("empty finding fix")
		}
		finding.Location = normalizeLocation(finding.Location, snapshot)
	}
	findings = append(findings, Check(snapshot)...)
	slices.SortFunc(findings, func(a, b Finding) int {
		ranks := map[string]int{"high": 0, "medium": 1, "low": 2}
		if n := ranks[a.Severity] - ranks[b.Severity]; n != 0 {
			return n
		}
		if n := strings.Compare(a.Location.File, b.Location.File); n != 0 {
			return n
		}
		if n := a.Location.Line - b.Location.Line; n != 0 {
			return n
		}
		if n := strings.Compare(a.Location.KataID, b.Location.KataID); n != 0 {
			return n
		}
		if n := strings.Compare(a.Message, b.Message); n != 0 {
			return n
		}
		return strings.Compare(a.Fix, b.Fix)
	})
	return slices.Compact(findings), nil
}

// A bad model anchor does not invalidate the finding itself. Keep a known
// artifact when only its line is invalid; discard unknown or ambiguous anchors.
func normalizeLocation(location Location, snapshot Snapshot) Location {
	if location.KataID != "" {
		if location.File != "" || location.Line != 0 {
			return Location{}
		}
		for _, issue := range snapshot.Issues {
			if issue.ShortID == location.KataID {
				return location
			}
		}
		return Location{}
	}
	for _, artifact := range snapshot.Artifacts {
		if artifact.Path == location.File {
			if location.Line < 1 || location.Line > strings.Count(artifact.Content, "\n")+1 {
				location.Line = 0
			}
			return location
		}
	}
	return Location{}
}

func formatLocation(location Location) string {
	if location.File != "" {
		if location.Line > 0 {
			return fmt.Sprintf("%s:%d", location.File, location.Line)
		}
		return location.File
	}
	if location.KataID != "" {
		return "Kata " + location.KataID
	}
	return ""
}

// Document preserves goal findings in the canonical stored review format.
func Document(findings []Finding) structuredreview.Document {
	doc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion,
		Summary:       "Goal review complete.",
		Verdict:       structuredreview.VerdictPass,
		Findings:      make([]structuredreview.Finding, len(findings)),
	}
	if len(findings) > 0 {
		doc.Verdict = structuredreview.VerdictFail
	}
	for i, finding := range findings {
		doc.Findings[i] = structuredreview.Finding{
			Severity: finding.Severity,
			Problem:  finding.Message,
			Fix:      finding.Fix,
			Location: formatLocation(finding.Location),
		}
	}
	return doc
}

// FindingsFromDocument restores goal locations against the frozen evidence.
// The caller must decode and validate the stored document first.
func FindingsFromDocument(doc structuredreview.Document, snapshot Snapshot) []Finding {
	findings := make([]Finding, len(doc.Findings))
	for i, finding := range doc.Findings {
		location := normalizeLocation(Location{File: finding.Location}, snapshot)
		if location.File == "" {
			if colon := strings.LastIndexByte(finding.Location, ':'); colon >= 0 {
				if line, err := strconv.Atoi(finding.Location[colon+1:]); err == nil {
					location = normalizeLocation(Location{File: finding.Location[:colon], Line: line}, snapshot)
				}
			}
		}
		if location.File == "" {
			if id, ok := strings.CutPrefix(finding.Location, "Kata "); ok {
				location = normalizeLocation(Location{KataID: id}, snapshot)
			}
		}
		findings[i] = Finding{
			Severity: finding.Severity,
			Message:  finding.Problem,
			Fix:      finding.Fix,
			Location: location,
		}
	}
	return findings
}

func Render(findings []Finding) string {
	if len(findings) == 0 {
		return "No issues found."
	}
	var result strings.Builder
	for _, finding := range findings {
		location := formatLocation(finding.Location)
		if location != "" {
			location += ": "
		}
		fmt.Fprintf(&result, "- %s: %s%s Fix: %s\n", finding.Severity, location, strings.ReplaceAll(finding.Message, "\n", " "), strings.ReplaceAll(finding.Fix, "\n", " "))
	}
	return strings.TrimSpace(result.String())
}
