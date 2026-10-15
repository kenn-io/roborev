// Package rebuttal defines the result of a focused finding re-evaluation.
package rebuttal

import (
	"encoding/json/v2"
	"fmt"
	"strings"
)

// Assessment is the agent's assertion about the selected original finding.
type Assessment struct {
	Outcome     string `json:"outcome"`
	Explanation string `json:"explanation"`
}

// Parse accepts one JSON assessment with evidence suitable for a Kata reply.
func Parse(output string) (Assessment, error) {
	var a Assessment
	output = strings.TrimSpace(output)
	if strings.HasPrefix(output, "```") {
		header, body, ok := strings.Cut(output, "\n")
		language := strings.TrimSpace(strings.TrimPrefix(header, "```"))
		if ok && (language == "" || strings.EqualFold(language, "json")) && strings.HasSuffix(body, "```") {
			output = strings.TrimSpace(strings.TrimSuffix(body, "```"))
		}
	}
	if err := json.Unmarshal([]byte(output), &a, json.RejectUnknownMembers(true)); err != nil {
		return a, fmt.Errorf("invalid rebuttal assessment: %w", err)
	}
	switch a.Outcome {
	case "confirmed", "refuted", "superseded":
	default:
		return a, fmt.Errorf("invalid rebuttal outcome %q", a.Outcome)
	}
	a.Explanation = strings.TrimSpace(a.Explanation)
	if a.Explanation == "" {
		return a, fmt.Errorf("rebuttal explanation must not be empty")
	}
	return a, nil
}
