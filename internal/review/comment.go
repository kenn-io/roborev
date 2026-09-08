package review

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// CommentConfig is the resolved publication policy. An empty threshold shows
// all findings; preparation never inherits policy from review data.
type CommentConfig struct {
	MinSeverity string
}

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
// supplied publication policy. It never changes the review or its findings.
func PrepareComment(cfg CommentConfig, r ReviewResult, sourceLabels []string) PreparedComment {
	comment := PreparedComment{minSeverity: strings.ToLower(strings.TrimSpace(cfg.MinSeverity))}
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

// splitProseFindings keeps each finding's Markdown intact. Markdown containers
// distinguish nested details from independent sections. Summary suppression
// lasts until a section boundary; rubric suppression comes only from the shared
// per-line classification, so it cannot spill into subsequent prose.
func splitProseFindings(output string) []proseFinding {
	var blocks []proseFinding
	var lines []string
	severity := ""
	findingLevel := 0
	var findingItem *ast.ListItem
	inSummary, summaryLevel := false, 0
	fence := ""
	flush := func() {
		if text := strings.TrimSpace(strings.Join(lines, "\n")); text != "" {
			blocks = append(blocks, proseFinding{severity: severity, text: text})
		}
		lines = nil
		severity = ""
		findingLevel = 0
		findingItem = nil
	}
	inputLines := strings.Split(output, "\n")
	labels := storage.ProseSeverityLabels(inputLines)
	layout := proseLineLayout(output, inputLines)
	for i, line := range inputLines {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if !inSummary {
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
			inSummary = false
			continue
		} else {
			level := layout[i].headingLevel
			section := storage.ProseSection(line)
			if inSummary {
				if section == "findings" || (level > 0 && (summaryLevel == 0 || level <= summaryLevel)) {
					inSummary = false
				} else {
					continue
				}
			}
			if section == "summary" {
				flush()
				inSummary, summaryLevel = true, level
				continue
			}
			if labels[i].Legend {
				flush()
				continue
			}
			if level > 0 {
				boundary := severity == "" ||
					(findingLevel > 0 && level <= findingLevel) ||
					(findingLevel == 0 && (findingItem == nil || findingItem != layout[i].listItem))
				if boundary {
					flush()
				}
			}
			if section == "findings" {
				continue
			}
			if label := labels[i].Severity; label != "" {
				flush()
				severity = label
				findingLevel = level
				findingItem = layout[i].listItem
			}
		}
		if !inSummary {
			lines = append(lines, line)
		}
	}
	flush()
	return blocks
}

type proseLineContext struct {
	headingLevel int
	listItem     *ast.ListItem
}

// proseLineLayout uses Markdown's parsed containers for structure, while the
// formatter retains the original source text. A heading indented inside a list
// item belongs to that item; an unindented heading starts a separate section.
func proseLineLayout(output string, lines []string) []proseLineContext {
	layout := make([]proseLineContext, len(lines))
	offsets := make([]int, len(lines))
	for i := 1; i < len(lines); i++ {
		offsets[i] = offsets[i-1] + len(lines[i-1]) + 1
	}
	root := goldmark.DefaultParser().Parse(text.NewReader([]byte(output)))
	var items []*ast.ListItem
	// The walker performs no fallible operations and always returns nil.
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if item, ok := node.(*ast.ListItem); ok {
			if entering {
				items = append(items, item)
			} else {
				items = items[:len(items)-1]
			}
		}
		if !entering || node.Type() != ast.TypeBlock {
			return ast.WalkContinue, nil
		}
		segments := node.Lines()
		for j := 0; j < segments.Len(); j++ {
			line, exact := slices.BinarySearch(offsets, segments.At(j).Start)
			if !exact {
				line--
			}
			if len(items) > 0 {
				layout[line].listItem = items[len(items)-1]
			}
			if heading, ok := node.(*ast.Heading); ok {
				layout[line].headingLevel = heading.Level
			}
		}
		return ast.WalkContinue, nil
	})
	return layout
}
