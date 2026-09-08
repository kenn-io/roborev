package review

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"

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

// splitProseFindings selects complete source spans using parsed Markdown
// blocks. Goldmark owns headings, containers, separators, and code syntax;
// the shared prose classifier only identifies review labels and rubrics.
func splitProseFindings(output string) []proseFinding {
	var blocks []proseFinding
	start, severity := 0, ""
	var finding, summary *proseBoundary
	flush := func(end int) {
		if body := strings.TrimSpace(output[start:end]); body != "" && summary == nil {
			blocks = append(blocks, proseFinding{severity: severity, text: body})
		}
		start, severity, finding = end, "", nil
	}
	for _, boundary := range proseBoundaries(output) {
		if summary != nil {
			if boundary.nestedIn(summary) ||
				(boundary.section != "findings" && !boundary.separator &&
					(boundary.level == 0 || (summary.level > 0 && boundary.level > summary.level))) {
				continue
			}
			start, summary = boundary.start, nil
		}
		// Explicit finding labels start a new finding even inside a later
		// list or quote. Unlabelled nested details retain their owner.
		if finding != nil && boundary.severity == "" && boundary.nestedIn(finding) {
			continue
		}
		switch {
		case boundary.section == "summary":
			flush(boundary.start)
			summary = &boundary
		case boundary.legend || boundary.separator || boundary.section == "findings":
			flush(boundary.start)
			start = boundary.end
		case boundary.severity != "":
			flush(boundary.start)
			severity, finding = boundary.severity, &boundary
		case boundary.level > 0 && (finding == nil || finding.level == 0 || boundary.level <= finding.level):
			flush(boundary.start)
		}
	}
	flush(len(output))
	return blocks
}

// proseBoundary describes a review marker in a parsed block. Offsets include
// the original Markdown markers, so extraction never needs to re-render it.
type proseBoundary struct {
	start, end        int
	level             int
	container         ast.Node
	section, severity string
	legend, separator bool
}

func (b proseBoundary) nestedIn(section *proseBoundary) bool {
	if b.container == section.container {
		// A paragraph introducing a finding inside a list item owns the
		// other blocks in that item. Document paragraphs do not own later
		// headings. Separate labels within one paragraph remain separable.
		return section.level == 0 && b.level > 0 && b.container.Kind() != ast.KindDocument
	}
	for parent := b.container.Parent(); parent != nil; parent = parent.Parent() {
		if parent == section.container {
			return true
		}
	}
	return false
}

func proseBoundaries(output string) []proseBoundary {
	lines := strings.Split(output, "\n")
	offsets := make([]int, len(lines))
	for i := 1; i < len(lines); i++ {
		offsets[i] = offsets[i-1] + len(lines[i-1]) + 1
	}
	semanticLines := make([]string, len(lines))
	lineAt := func(pos int) int {
		line, exact := slices.BinarySearch(offsets, pos)
		if !exact {
			line--
		}
		return line
	}
	var boundaries []proseBoundary
	root := parser.New().Parse([]byte(output))
	// Only headings and prose can supply review labels. Code and HTML blocks
	// remain opaque source spans, regardless of their contents or delimiters.
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *ast.Heading, *ast.Paragraph:
			block := node.(ast.BlockNode)
			for _, segment := range block.Source() {
				line := lineAt(segment.Start)
				// Source excludes list and quote markers. Classify that text,
				// while retaining the original offsets for publication.
				semanticLines[line] = strings.TrimSuffix(output[segment.Start:segment.Stop], "\n")
				b := proseBoundary{
					start: offsets[line], end: min(offsets[line]+len(lines[line])+1, len(output)),
					container: node.Parent(),
				}
				if heading, ok := node.(*ast.Heading); ok {
					b.level = heading.Level
					semanticLines[line] = "# " + semanticLines[line]
					lastLine := lineAt(block.Source()[len(block.Source())-1].Start)
					if heading.HeadingKind == ast.HeadingKindSetext {
						lastLine++ // Include the parsed heading's underline.
					}
					b.end = min(offsets[lastLine]+len(lines[lastLine])+1, len(output))
				}
				boundaries = append(boundaries, b)
				if b.level > 0 {
					break // A multiline heading is one section boundary.
				}
			}
			return ast.WalkSkipChildren, nil
		case *ast.ThematicBreak:
			line := lineAt(n.Pos())
			boundaries = append(boundaries, proseBoundary{
				start: offsets[line], end: min(offsets[line]+len(lines[line])+1, len(output)),
				container: node.Parent(), separator: true,
			})
		}
		return ast.WalkContinue, nil
	})
	labels := storage.ProseSeverityLabels(semanticLines)
	markers := boundaries[:0]
	for _, b := range boundaries {
		line := lineAt(b.start)
		b.section = storage.ProseSection(semanticLines[line])
		b.severity, b.legend = labels[line].Severity, labels[line].Legend
		if b.level > 0 || b.section != "" || b.severity != "" || b.legend || b.separator {
			markers = append(markers, b)
		}
	}
	return markers
}
