package provenance

import (
	"fmt"
	"strings"
)

// FormatText presents stored evidence without querying the current provider.
// Callers escape it for prompts or sanitize terminal controls for display.
func FormatText(commits []Commit) string {
	if len(commits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Agent sessions behind these commits\n")
	for _, c := range commits {
		coverage := c.Coverage
		switch coverage {
		case "why-selected":
			coverage = "agentsview why selection: up to three sessions, one source card per session"
		case "http":
			coverage = "agentsview commit lookup"
		default:
			coverage = "trailers only; archive enrichment unavailable or not queried"
		}
		fmt.Fprintf(&b, "\nCommit %s (%s)\n", c.CommitSHA, coverage)
		for _, s := range c.Sessions {
			fmt.Fprintf(&b, "Session %s (%s; %s", s.SessionID, s.Agent, s.Source)
			if s.Ambiguous {
				b.WriteString("; ambiguous commit match")
			}
			switch s.Availability {
			case "gone":
				b.WriteString("; session unavailable")
			case "unknown":
				b.WriteString("; availability unknown")
			}
			b.WriteString(")\n")
			if !s.Resolved || s.CommitSHA != c.CommitSHA {
				fmt.Fprintf(&b, "Evidence commit: %s (resolved: %t)\n", s.CommitSHA, s.Resolved)
			}
			if s.WebURL != "" {
				fmt.Fprintf(&b, "Session URL: %s\n", s.WebURL)
			}
			if s.Turn != nil {
				fmt.Fprintf(&b, "Writing prompt %d (%s", s.Turn.Ordinal, s.Turn.Why)
				if s.Turn.At != "" {
					fmt.Fprintf(&b, "; %s", s.Turn.At)
				}
				fmt.Fprintf(&b, "):\n%s\n", s.Turn.Prompt)
			}
		}
	}
	return b.String()
}
