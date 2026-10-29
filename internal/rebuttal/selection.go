package rebuttal

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// Selection binds one quoted excerpt to the immutable version of its review.
// It is stored in the existing task prompt, not in comments or a new table.
type Selection struct {
	ReviewUUID string `json:"review_uuid"`
	Finding    string `json:"finding"`
}

const selectionPrefix = "roborev-rebuttal-selection-v1:"

// EncodeSelection prepends an anchored metadata line before any quoted input.
func EncodeSelection(s Selection, body string) (string, error) {
	if strings.TrimSpace(s.ReviewUUID) == "" || strings.TrimSpace(s.Finding) == "" || !utf8.ValidString(s.ReviewUUID) || !utf8.ValidString(s.Finding) {
		return "", fmt.Errorf("selection requires a review version and finding")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return selectionPrefix + base64.RawStdEncoding.EncodeToString(raw) + "\n" + body, nil
}

// DecodeSelection accepts only the first line. A marker in quoted input cannot
// create a version binding for an old prompt.
func DecodeSelection(prompt string) (Selection, error) {
	line, _, ok := strings.Cut(prompt, "\n")
	if !ok || !strings.HasPrefix(line, selectionPrefix) {
		return Selection{}, fmt.Errorf("unbound rebuttal selection")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(line, selectionPrefix))
	if err != nil {
		return Selection{}, err
	}
	var s Selection
	if err = json.Unmarshal(raw, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	if strings.TrimSpace(s.ReviewUUID) == "" || strings.TrimSpace(s.Finding) == "" {
		return Selection{}, fmt.Errorf("invalid selection")
	}
	return s, nil
}

// MatchFinding returns the index of a unique whitespace-normalized containment
// match, or -1 for an absent or ambiguous excerpt. Finding indexes are local to
// this version of the document; they are not persistent finding identities.
func MatchFinding(excerpt string, findings []structuredreview.Finding) int {
	excerpt = strings.Join(strings.Fields(excerpt), " ")
	if excerpt == "" {
		return -1
	}
	match := -1
	for i, f := range findings {
		// Use the canonical Markdown fields and original positional heading, so
		// selections copied from either the TUI or browser can match.
		text := f.Markdown(i + 1)
		if strings.Contains(strings.Join(strings.Fields(text), " "), excerpt) {
			if match != -1 {
				return -1
			}
			match = i
		}
	}
	return match
}
