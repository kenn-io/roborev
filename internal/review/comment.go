package review

import (
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// PreparedComment is a publication-only copy of a review. Its fields are private
// so formatting receives only findings selected by PrepareComment.
type PreparedComment struct {
	minSeverity string
	structured  bool
	verbatim    *string
	findings    []commentFinding
}

type commentFinding struct {
	severity string
	problem  string
	fix      string
	location string
	sources  []string
	markdown string
}

// PrepareComment ingests structured or prose review data and applies the
// result's resolved threshold. It never changes the review or its findings.
func PrepareComment(r ReviewResult, sourceLabels []string) PreparedComment {
	comment := PreparedComment{minSeverity: strings.ToLower(strings.TrimSpace(r.MinSeverity))}
	doc := r.Structured
	if doc == nil && len(r.StructuredOutput) > 0 {
		if decoded, err := DecodeStructuredReview(r.StructuredOutput); err == nil {
			doc = &decoded
		}
	}
	var findings []commentFinding
	if doc != nil {
		if sourceLabels == nil {
			sourceLabels = doc.SourceLabels
		}
		comment.structured = true
		for _, finding := range doc.Findings {
			var sources []string
			for _, n := range finding.Sources {
				if n > 0 && n <= len(sourceLabels) {
					label := sourceLabels[n-1]
					if label != "" && !slices.Contains(sources, label) {
						sources = append(sources, label)
					}
				}
			}
			findings = append(findings, commentFinding{
				severity: finding.Severity, problem: finding.Problem,
				fix: finding.Fix, location: finding.Location, sources: sources,
			})
		}
	} else {
		// Publication supplies the current header; a stored wrapper is content
		// to ingest, not permission to bypass the display filter.
		if output := strings.TrimSpace(r.Output); strings.HasPrefix(output, "## roborev:") {
			_, r.Output, _ = strings.Cut(output, "\n")
			r.Output = strings.TrimLeft(r.Output, "\r\n")
		}
		// With no display filter, prose passes through exactly as authored.
		if config.SeverityRank(comment.minSeverity) <= config.SeverityRank("low") {
			comment.verbatim = &r.Output
			return comment
		}
		labelled := false
		for _, block := range splitProseFindings(r.Output) {
			labelled = labelled || block.severity != ""
			findings = append(findings, commentFinding{severity: block.severity, markdown: block.text})
		}
		if !labelled {
			comment.verbatim = &r.Output
			return comment
		}
	}
	threshold := config.SeverityRank(comment.minSeverity)
	for _, finding := range findings {
		if finding.severity == "" || config.SeverityRank(finding.severity) >= threshold {
			comment.findings = append(comment.findings, finding)
		}
	}
	return comment
}

// FormatComment renders prepared findings. It does not resolve configuration,
// parse review output, or filter findings.
func FormatComment(comment PreparedComment) string {
	if comment.verbatim != nil {
		return *comment.verbatim
	}
	if len(comment.findings) == 0 {
		if comment.minSeverity != "" && comment.minSeverity != "low" {
			return fmt.Sprintf("**Verdict:** No findings at or above %s severity.\n", comment.minSeverity)
		}
		return "**Verdict:** No issues found.\n"
	}
	if !comment.structured {
		blocks := make([]string, len(comment.findings))
		for i, finding := range comment.findings {
			blocks[i] = finding.markdown
		}
		return strings.Join(blocks, "\n\n---\n\n")
	}
	var out strings.Builder
	noun := "findings"
	if len(comment.findings) == 1 {
		noun = "finding"
	}
	fmt.Fprintf(&out, "**Verdict:** Changes require fixes for %d %s.", len(comment.findings), noun)
	for _, severity := range []string{"critical", "high", "medium", "low"} {
		heading := false
		for _, finding := range comment.findings {
			if finding.severity != severity {
				continue
			}
			if !heading {
				fmt.Fprintf(&out, "\n\n### %s\n", strings.ToUpper(severity[:1])+severity[1:])
				heading = true
			}
			out.WriteString("\n- ")
			if finding.location != "" {
				fmt.Fprintf(&out, "%s: ", finding.location)
			}
			fmt.Fprintf(&out, "%s %s", finding.problem, finding.fix)
			if len(finding.sources) > 0 {
				fmt.Fprintf(&out, "\n\n  *Reported by: %s*", strings.Join(finding.sources, ", "))
			}
			out.WriteString("\n")
		}
	}
	return out.String()
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
	inputLines := strings.Split(output, "\n")
	sectionStart := 0
	for i, line := range inputLines {
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
			sectionStart = i + 1
			continue
		} else {
			heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
			heading = strings.ReplaceAll(heading, "**", "")
			heading = strings.TrimSpace(strings.TrimLeft(heading, "-*•0123456789.) "))
			if heading == "summary" || strings.HasPrefix(heading, "summary:") {
				flush()
				collect = false
				continue
			}
			if heading == "findings" || heading == "review findings" || heading == "review findings:" {
				collect = true
				sectionStart = i + 1
				continue
			}
			if label, legend := storage.SeverityLabelAt(inputLines[sectionStart:], i-sectionStart); label != "" {
				flush()
				if legend {
					collect = false
					continue
				}
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
