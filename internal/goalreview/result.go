package goalreview

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

type Location struct {
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	KataID string `json:"kata_id,omitempty"`
}

type Finding struct {
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
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
	for _, finding := range findings {
		if finding.Severity != "high" && finding.Severity != "medium" && finding.Severity != "low" {
			return nil, fmt.Errorf("invalid finding severity")
		}
		if strings.TrimSpace(finding.Message) == "" {
			return nil, fmt.Errorf("empty finding message")
		}
		location := finding.Location
		if location.KataID != "" {
			if location.File != "" || location.Line != 0 {
				return nil, fmt.Errorf("finding must have one location")
			}
			known := false
			for _, issue := range snapshot.Issues {
				if issue.ShortID == location.KataID {
					known = true
					break
				}
			}
			if !known {
				return nil, fmt.Errorf("unknown Kata finding location")
			}
		} else {
			valid := false
			for _, artifact := range snapshot.Artifacts {
				if artifact.Path == location.File && location.Line > 0 && location.Line <= len(strings.Split(artifact.Content, "\n")) {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("unknown artifact or out-of-range finding line")
			}
		}
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
		return strings.Compare(a.Message, b.Message)
	})
	return slices.Compact(findings), nil
}

func Render(findings []Finding) string {
	if len(findings) == 0 {
		return "No issues found."
	}
	var result strings.Builder
	for _, finding := range findings {
		location := finding.Location.KataID
		if finding.Location.File != "" {
			location = fmt.Sprintf("%s:%d", finding.Location.File, finding.Location.Line)
		}
		fmt.Fprintf(&result, "- %s: %s: %s\n", finding.Severity, location, strings.ReplaceAll(finding.Message, "\n", " "))
	}
	return strings.TrimSpace(result.String())
}
