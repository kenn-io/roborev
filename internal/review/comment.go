package review

import (
	"fmt"
	"regexp"
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
	ShowAll     bool
	// MinConfidence filters stated structured confidence; zero disables it.
	MinConfidence float64
	// MaxFindings limits structured findings; zero leaves them unlimited.
	MaxFindings int
	// FullReviewReference points to the complete review when findings are omitted.
	FullReviewReference string
}

// PreparedComment is a publication-only copy of a review. Its fields are private
// so formatting receives only findings selected by PrepareComment.
type PreparedComment struct {
	minSeverity         string
	passed              bool
	confidenceFiltered  bool
	actionable          int
	legacy              bool
	structured          bool
	verbatim            *string
	findings            []commentFinding
	nits                []commentFinding
	unlikely            []commentFinding
	omitted             int
	fullReviewReference string
}

type commentFinding struct {
	severity string
	problem  string
	fix      string
	location string
	markdown string
}

// PrepareComment ingests structured or prose review data and applies the
// supplied publication policy. It never changes the review or its findings.
func PrepareComment(cfg CommentConfig, r ReviewResult) PreparedComment {
	comment := PreparedComment{minSeverity: strings.ToLower(strings.TrimSpace(cfg.MinSeverity)), fullReviewReference: cfg.FullReviewReference}
	threshold := config.SeverityRank(comment.minSeverity)
	verdictThreshold := threshold
	doc := r.Structured
	if doc == nil && len(r.StructuredOutput) > 0 {
		if decoded, err := DecodeStructuredReview(r.StructuredOutput); err == nil {
			doc = &decoded
		}
	}
	if doc != nil && doc.Legacy != nil {
		comment.legacy = true
		r.Output = doc.Legacy.Markdown
		doc = nil
	}
	if doc != nil {
		effective := comment.minSeverity
		if effective == "" {
			effective = r.MinSeverity
		}
		comment.passed = !doc.UnableToReview() && doc.Passed(effective)
		if cfg.ShowAll {
			comment.minSeverity, threshold, cfg.MinConfidence, cfg.MaxFindings = "", 0, 0, 0
		}
		comment.structured = true
		findings := slices.Clone(doc.Findings)
		shouldFold := func(finding StructuredFinding) bool {
			return !cfg.ShowAll && finding.Topic != "security" && finding.Severity != "high" && finding.Severity != "critical" && (finding.Nit || finding.Unlikely)
		}
		slices.SortStableFunc(findings, func(a, b StructuredFinding) int {
			if shouldFold(a) != shouldFold(b) {
				if shouldFold(a) {
					return 1
				}
				return -1
			}
			return config.SeverityRank(b.Severity) - config.SeverityRank(a.Severity)
		})
		selected := 0
		for _, finding := range findings {
			if finding.Severity != "" && config.SeverityRank(finding.Severity) < threshold {
				continue
			}
			// Stored findings carry kind and confidence together.
			if finding.Kind != "" && finding.Confidence < cfg.MinConfidence {
				comment.confidenceFiltered = true
				continue
			}
			fold := shouldFold(finding)
			if selected++; cfg.MaxFindings > 0 && selected > cfg.MaxFindings {
				comment.omitted++
				continue
			}
			if config.SeverityRank(finding.Severity) >= verdictThreshold {
				comment.actionable++
			}
			item := commentFinding{severity: finding.Severity, problem: finding.Problem, fix: finding.Fix, location: finding.Location}
			if fold && finding.Nit {
				comment.nits = append(comment.nits, item)
			} else if fold && finding.Unlikely {
				comment.unlikely = append(comment.unlikely, item)
			} else {
				comment.findings = append(comment.findings, item)
			}
		}
	} else {
		// Publication supplies the current header; a stored wrapper is content
		// to ingest, not permission to bypass the display filter.
		if output := strings.TrimSpace(r.Output); strings.HasPrefix(output, "## roborev:") {
			_, r.Output, _ = strings.Cut(output, "\n")
			r.Output = strings.TrimLeft(r.Output, "\r\n")
		}
		// With no display filter, prose passes through exactly as authored.
		if threshold <= config.SeverityRank("low") {
			comment.verbatim = &r.Output
			return comment
		}
		labelled := false
		for _, block := range splitProseFindings(r.Output) {
			labelled = labelled || block.severity != ""
			if block.severity == "" || config.SeverityRank(block.severity) >= threshold {
				comment.findings = append(comment.findings, commentFinding{severity: block.severity, markdown: block.text})
			}
		}
		if !labelled {
			comment.verbatim = &r.Output
			comment.findings = nil
			return comment
		}
	}
	return comment
}

// IsClean reports whether filtering proved there is nothing to publish.
// Verbatim prose has no reliable finding metadata and remains publishable.
func (comment PreparedComment) IsClean() bool {
	return !comment.legacy && comment.verbatim == nil && (comment.structured && comment.actionable == 0 || !comment.structured && len(comment.findings) == 0)
}

// FormatComment renders prepared findings. It does not resolve configuration,
// parse review output, or filter findings.
func FormatComment(comment PreparedComment) string {
	body, _ := FormatCommentWithFold(comment)
	return body
}

// CommentFold records the start of a generated fold.
type CommentFold struct {
	Start int
}

// Shift returns fold offsets adjusted for a prefix.
func (f *CommentFold) Shift(prefix int) *CommentFold {
	if f == nil {
		return nil
	}
	return &CommentFold{Start: f.Start + prefix}
}

// FormatCommentWithFold also returns the start of the generated fold.
func FormatCommentWithFold(comment PreparedComment) (string, *CommentFold) {
	if comment.verbatim != nil {
		return *comment.verbatim, nil
	}
	if len(comment.findings) == 0 && len(comment.nits)+len(comment.unlikely) == 0 {
		if comment.confidenceFiltered {
			return "**Verdict:** No findings match the comment filters.\n", nil
		}
		if comment.minSeverity != "" && comment.minSeverity != "low" {
			return fmt.Sprintf("**Verdict:** No findings at or above %s severity.\n", comment.minSeverity), nil
		}
		return "**Verdict:** No issues found.\n", nil
	}
	if !comment.structured {
		blocks := make([]string, len(comment.findings))
		for i, finding := range comment.findings {
			blocks[i] = finding.markdown
		}
		return strings.Join(blocks, "\n\n---\n\n"), nil
	}
	var out strings.Builder
	if comment.passed {
		out.WriteString("**Verdict:** No fixes needed before merge.")
	} else {
		out.WriteString("**Verdict:** Changes require fixes before merge.")
	}
	folded := len(comment.nits) + len(comment.unlikely)
	fmt.Fprintf(&out, "\n\n%d findings shown; %d folded (nits/unlikely)", len(comment.findings)+folded, folded)
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
			// Reviewer attribution belongs in the comment footer, not on
			// each finding.
			renderCommentFinding(&out, finding)
		}
	}
	var fold *CommentFold
	if len(comment.nits)+len(comment.unlikely) > 0 {
		groups := []struct {
			title, one, many string
			findings         []commentFinding
		}{{"Nits", "nit", "nits", comment.nits}, {"Unlikely findings", "unlikely finding", "unlikely findings", comment.unlikely}}
		var labels []string
		for _, group := range groups {
			if n := len(group.findings); n == 1 {
				labels = append(labels, "1 "+group.one)
			} else if n > 1 {
				labels = append(labels, fmt.Sprintf("%d %s", n, group.many))
			}
		}
		fold = &CommentFold{Start: out.Len()}
		fmt.Fprintf(&out, "\n\n<details><summary>%s hidden</summary>", strings.Join(labels, " and "))
		out.WriteString("\n")
		for _, group := range groups {
			if len(group.findings) > 0 {
				fmt.Fprintf(&out, "\n#### %s\n", group.title)
			}
			for _, finding := range group.findings {
				var item strings.Builder
				renderCommentFinding(&item, finding)
				out.WriteString(escapeFoldClose(item.String()))
			}
		}
		out.WriteString("\n</details>\n")
	}
	if notice := comment.OmissionNotice(); notice != "" {
		return strings.TrimRight(out.String(), "\n") + notice, fold
	}
	return out.String(), fold
}

var foldCloseTag = regexp.MustCompile(`(?i)</?details(?:\s[^>]*|/?)>`)

func escapeFoldClose(s string) string {
	return foldCloseTag.ReplaceAllStringFunc(s, func(tag string) string {
		return "&lt;" + tag[1:len(tag)-1] + "&gt;"
	})
}

func renderCommentFinding(out *strings.Builder, finding commentFinding) {
	out.WriteString("\n- ")
	if finding.location != "" {
		fmt.Fprintf(out, "`%s`: ", finding.location)
	}
	out.WriteString(finding.problem)
	if finding.fix != "" {
		fmt.Fprintf(out, "\n\n  **Fix:** %s", finding.fix)
	}
	out.WriteString("\n")
}

// OmissionNotice returns the cap explanation separately so publishers can
// reserve it alongside metadata when fitting the final transport byte limit.
func (comment PreparedComment) OmissionNotice() string {
	if comment.omitted == 0 {
		return ""
	}
	noun := "findings"
	if comment.omitted == 1 {
		noun = "finding"
	}
	notice := fmt.Sprintf("\n\n%d more %s in the full review", comment.omitted, noun)
	if comment.fullReviewReference != "" {
		notice += fmt.Sprintf(": `%s`", comment.fullReviewReference)
	}
	return notice + "\n"
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
		if b.level == 0 {
			// A stamped "### Title" is a paragraph to Goldmark, not a heading.
			b.level = storage.StampedHeadingLevel(semanticLines[line])
		}
		b.section = storage.ProseSection(semanticLines[line])
		// A stamped "---" is a paragraph to Goldmark, not a thematic break.
		b.separator = b.separator || b.section == "separator"
		b.severity, b.legend = labels[line].Severity, labels[line].Legend
		if b.level > 0 || b.section != "" || b.severity != "" || b.legend || b.separator {
			markers = append(markers, b)
		}
	}
	return markers
}
