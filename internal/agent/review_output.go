package agent

import "strings"

// trailingReviewText keeps only the final assistant text segment after the most
// recent tool event. Under roborev's review contract, any assistant text before
// a later tool call is provisional working output rather than the persisted
// review body.
type trailingReviewText struct {
	parts     []string
	indexByID map[string]int
}

func newTrailingReviewText() *trailingReviewText {
	return &trailingReviewText{indexByID: make(map[string]int)}
}

func (t *trailingReviewText) Add(text string) {
	if text == "" {
		return
	}
	t.parts = append(t.parts, text)
}

func (t *trailingReviewText) AddWithID(id, text string) {
	if text == "" {
		return
	}
	if id == "" {
		t.parts = append(t.parts, text)
		return
	}
	if idx, ok := t.indexByID[id]; ok {
		t.parts[idx] = text
		return
	}
	t.indexByID[id] = len(t.parts)
	t.parts = append(t.parts, text)
}

func (t *trailingReviewText) ResetAfterTool() {
	t.parts = nil
	if len(t.indexByID) > 0 {
		t.indexByID = make(map[string]int)
	}
}

func (t *trailingReviewText) Join(sep string) string {
	return strings.Join(t.parts, sep)
}

// Last returns the final message in first-seen order, so a late update to an
// earlier message cannot displace it.
func (t *trailingReviewText) Last() string {
	if len(t.parts) == 0 {
		return ""
	}
	return t.parts[len(t.parts)-1]
}
