package review

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// CommentMarkdown applies the display threshold without changing review data.
// Prose findings use the separators and severity labels requested by review
// prompts. Unlabelled text cannot be assigned a severity and remains visible.
func (r ReviewResult) CommentMarkdown() string {
	if r.Structured != nil {
		return r.Structured.CommentMarkdown(r.MinSeverity)
	}
	threshold := config.SeverityRank(strings.ToLower(strings.TrimSpace(r.MinSeverity)))
	if threshold <= config.SeverityRank("low") {
		return r.Output
	}
	blocks := splitProseFindings(r.Output)
	labelled := false
	var visible []string
	for _, block := range blocks {
		if block.severity != "" {
			labelled = true
			if config.SeverityRank(block.severity) < threshold {
				continue
			}
		}
		visible = append(visible, block.text)
	}
	if !labelled {
		return r.Output
	}
	if len(visible) == 0 {
		return fmt.Sprintf("**Verdict:** No findings at or above %s severity.\n", strings.ToLower(strings.TrimSpace(r.MinSeverity)))
	}
	return strings.Join(visible, "\n\n---\n\n")
}

type proseFinding struct {
	severity string
	text     string
}

// splitProseFindings keeps each finding's Markdown intact. A severity label
// starts a finding; the prompt's horizontal rule ends it. Explicit summary
// sections are excluded because they may describe findings hidden by the filter.
func splitProseFindings(output string) []proseFinding {
	var blocks []proseFinding
	var lines []string
	severity := ""
	collect := true
	fence := ""
	flush := func() {
		if text := strings.TrimSpace(strings.Join(lines, "\n")); text != "" {
			blocks = append(blocks, proseFinding{severity: severity, text: text})
		}
		lines = nil
		severity = ""
	}
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if collect {
				lines = append(lines, line)
			}
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]+" \t") == "" {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, trimmed[:1]))]
		} else if trimmed == "---" {
			flush()
			collect = true
			continue
		} else {
			heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
			heading = strings.ReplaceAll(heading, "**", "")
			if heading == "summary" || strings.HasPrefix(heading, "summary:") {
				flush()
				collect = false
				continue
			}
			if heading == "findings" || heading == "review findings" || heading == "review findings:" {
				collect = true
				continue
			}
			if label := storage.HighestSeverityLabel(line); label != "" {
				flush()
				severity = label
				collect = true
			}
		}
		if collect {
			lines = append(lines, line)
		}
	}
	flush()
	return blocks
}
