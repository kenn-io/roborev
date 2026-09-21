package structuredreview

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MarkdownRefusalReason is a stable code for why ParseMarkdown did not convert
// a review. Callers count refusals by this code.
type MarkdownRefusalReason string

const (
	// RefusalUnrecognizedFormat means the text does not start like any review
	// format roborev wrote.
	RefusalUnrecognizedFormat MarkdownRefusalReason = "unrecognized_format"
	// RefusalUnrecognizedContent means the text starts like a known format but
	// holds content outside its structure. Converting would lose or misplace
	// that content.
	RefusalUnrecognizedContent MarkdownRefusalReason = "unrecognized_content"
	RefusalMissingSummary      MarkdownRefusalReason = "missing_summary"
	RefusalMissingSeverity     MarkdownRefusalReason = "missing_severity"
	RefusalInvalidSeverity     MarkdownRefusalReason = "invalid_severity"
	RefusalMissingProblem      MarkdownRefusalReason = "missing_problem"
	RefusalMissingFix          MarkdownRefusalReason = "missing_fix"
	// RefusalUnrecoverableSources means a finding names reviewers that cannot
	// be matched to exactly one input review each.
	RefusalUnrecoverableSources MarkdownRefusalReason = "unrecoverable_sources"
)

// MarkdownRefusal reports that ParseMarkdown converted nothing, and why.
type MarkdownRefusal struct {
	Reason MarkdownRefusalReason
	Detail string
}

func (r *MarkdownRefusal) Error() string {
	return fmt.Sprintf("review Markdown not converted (%s): %s", r.Reason, r.Detail)
}

func refuse(reason MarkdownRefusalReason, format string, args ...any) *MarkdownRefusal {
	return &MarkdownRefusal{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// ParseMarkdownOptions carries facts the Markdown does not state.
type ParseMarkdownOptions struct {
	// SourceLabels names the input reviews of a synthesis, in review-number
	// order. ParseMarkdown needs them to turn a "Reported by" line back into
	// source numbers.
	SourceLabels []string
}

// ParseMarkdown converts review Markdown that roborev itself wrote back into
// a Document. It reads a fixed structure and never interprets prose: every
// severity, problem, fix, location, and source in the result is stated in the
// text. When the text does not fit a known structure exactly, or a finding
// lacks a severity, problem, or fix, it returns a *MarkdownRefusal and no
// document. It never drops text to make a review fit.
//
// It recognizes three forms:
//
//   - The output of Document.Markdown. The result is accepted only when
//     rendering it again reproduces the input, so d.Markdown("") always
//     converts back to d.
//   - The findings list older review prompts asked for: a "## Review Findings"
//     heading, one group of Severity, Location, Problem, and Fix bullets per
//     finding, and a "## Summary" section.
//   - A review whose first or last line is "No issues found.". The remaining
//     text is the summary.
//
// The older forms never stated the agent's own verdict, so their documents
// use schema version 1, which has none. Only a rendered "Agent assessment"
// line produces a version 2 document with a verdict.
func ParseMarkdown(markdown string, opts ParseMarkdownOptions) (Document, error) {
	text := strings.TrimSpace(strings.ReplaceAll(markdown, "\r\n", "\n"))
	if text == "" {
		return Document{}, refuse(RefusalUnrecognizedFormat, "the review is empty")
	}
	lines := strings.Split(text, "\n")
	var doc Document
	var refusal *MarkdownRefusal
	switch {
	case lines[0] == "## Summary":
		doc, refusal = parseRenderedMarkdown(text, lines, opts)
	case legacyFindingsHeading.MatchString(lines[0]):
		doc, refusal = parseFindingsList(lines)
	default:
		doc, refusal = parseNoIssues(lines)
	}
	if refusal != nil {
		return Document{}, refusal
	}
	return doc, nil
}

var (
	renderedFindingHeading = regexp.MustCompile(`^### (\d+)\. (Critical|High|Medium|Low)$`)
	renderedThresholdLine  = regexp.MustCompile(`^No findings at or above (critical|high|medium) severity\.$`)
	legacyFindingsHeading  = regexp.MustCompile(`^##\s+Review Findings\s*$`)
	legacySummaryHeading   = regexp.MustCompile(`^##\s+Summary\s*$`)
	legacyFieldBullet      = regexp.MustCompile(`^[-*]\s+\*\*(Severity|Location|Problem|Fix)(?:\*\*\s*:|:\*\*)\s*(.*)$`)
	legacySeparator        = regexp.MustCompile(`^-{3,}\s*$`)
	markdownHeading        = regexp.MustCompile(`^#{1,6}\s`)
	noIssuesLine           = regexp.MustCompile(`(?i)^no issues found\.?$`)
	summaryLabel           = regexp.MustCompile(`(?i)^summary\s*:\s*`)
)

const (
	renderedAssessment = "**Agent assessment:** "
	renderedLocation   = "**Location:** "
	renderedProblem    = "**Problem:** "
	renderedFix        = "**Fix:** "
	renderedReportedBy = "**Reported by:** "
	renderedNoIssues   = "No issues found."
)

// parseRenderedMarkdown reads the output of Document.Markdown. Summary,
// problem, and fix are free text that may itself look like structure, so the
// parse is only a candidate: it is accepted when rendering it reproduces the
// input exactly.
func parseRenderedMarkdown(text string, lines []string, opts ParseMarkdownOptions) (Document, *MarkdownRefusal) {
	var first *MarkdownRefusal
	for _, findingsAt := range renderedFindingsCandidates(lines) {
		doc, minSeverity, refusal := parseRenderedCandidate(lines, findingsAt, opts)
		if refusal != nil {
			if first == nil {
				first = refusal
			}
			continue
		}
		if strings.TrimSpace(doc.Markdown(minSeverity)) == text {
			return doc, nil
		}
	}
	if first != nil {
		return Document{}, first
	}
	return Document{}, refuse(RefusalUnrecognizedFormat,
		"the text starts with a Summary heading but is not what roborev renders for a review document")
}

// renderedFindingsCandidates lists where the findings section may start: each
// "## Findings" line, then -1 for a review without findings.
func renderedFindingsCandidates(lines []string) []int {
	var candidates []int
	for i, line := range lines {
		if line == "## Findings" {
			candidates = append(candidates, i)
		}
	}
	return append(candidates, -1)
}

func parseRenderedCandidate(lines []string, findingsAt int, opts ParseMarkdownOptions) (Document, string, *MarkdownRefusal) {
	header := lines[1:]
	if findingsAt >= 0 {
		header = lines[1:findingsAt]
	}
	header = trimBlankLines(header)
	minSeverity := ""
	if findingsAt >= 0 && len(header) > 0 {
		if m := renderedThresholdLine.FindStringSubmatch(header[len(header)-1]); m != nil {
			minSeverity = m[1]
			header = trimBlankLines(header[:len(header)-1])
		}
	}
	if findingsAt < 0 && len(header) > 0 && header[len(header)-1] == renderedNoIssues {
		header = trimBlankLines(header[:len(header)-1])
	}
	doc := Document{SchemaVersion: 1, Findings: []Finding{}}
	if len(header) > 0 {
		if label, ok := strings.CutPrefix(header[len(header)-1], renderedAssessment); ok {
			verdict, known := verdictFromLabel(label)
			if !known {
				return Document{}, "", refuse(RefusalUnrecognizedContent, "unknown agent assessment %q", label)
			}
			doc.Verdict = verdict
			doc.SchemaVersion = SchemaVersion
			header = trimBlankLines(header[:len(header)-1])
		}
	}
	doc.Summary = strings.TrimSpace(strings.Join(header, "\n"))
	if doc.Summary == "" {
		return Document{}, "", refuse(RefusalMissingSummary, "the Summary section is empty")
	}
	if findingsAt < 0 {
		// Without this check a Findings section that failed to parse would be
		// kept as summary text of a review that claims no findings.
		if slices.Contains(header, "## Findings") {
			return Document{}, "", refuse(RefusalUnrecognizedContent,
				"the Findings section is not what roborev renders for findings")
		}
		return doc, "", nil
	}

	var blocks [][]string
	for _, line := range lines[findingsAt+1:] {
		if m := renderedFindingHeading.FindStringSubmatch(line); m != nil && m[1] == strconv.Itoa(len(blocks)+1) {
			blocks = append(blocks, []string{line})
			continue
		}
		if len(blocks) == 0 {
			if strings.TrimSpace(line) != "" {
				return Document{}, "", refuse(RefusalUnrecognizedContent, "text between the Findings heading and the first finding")
			}
			continue
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], line)
	}
	if len(blocks) == 0 {
		return Document{}, "", refuse(RefusalUnrecognizedContent, "a Findings heading without findings")
	}
	for i, block := range blocks {
		finding, refusal := parseRenderedFinding(i+1, block, opts.SourceLabels)
		if refusal != nil {
			return Document{}, "", refusal
		}
		if len(finding.Sources) > 0 {
			doc.SourceLabels = opts.SourceLabels
		}
		doc.Findings = append(doc.Findings, finding)
	}
	return doc, minSeverity, nil
}

func parseRenderedFinding(number int, block []string, sourceLabels []string) (Finding, *MarkdownRefusal) {
	m := renderedFindingHeading.FindStringSubmatch(block[0])
	finding := Finding{Severity: strings.ToLower(m[2])}
	body := trimBlankLines(block[1:])
	if len(body) > 0 {
		if location, ok := strings.CutPrefix(body[0], renderedLocation); ok {
			finding.Location = strings.TrimSpace(location)
			body = trimBlankLines(body[1:])
		}
	}
	if len(body) > 0 {
		if labels, ok := strings.CutPrefix(body[len(body)-1], renderedReportedBy); ok {
			sources, refusal := sourcesFromLabels(number, labels, sourceLabels)
			if refusal != nil {
				return Finding{}, refusal
			}
			finding.Sources = sources
			body = trimBlankLines(body[:len(body)-1])
		}
	}
	if len(body) == 0 || !strings.HasPrefix(body[0], renderedProblem) {
		return Finding{}, refuse(RefusalMissingProblem, "finding %d has no Problem field", number)
	}
	fixAt := slices.IndexFunc(body, func(line string) bool { return strings.HasPrefix(line, renderedFix) })
	if fixAt < 0 {
		return Finding{}, refuse(RefusalMissingFix, "finding %d has no Fix field", number)
	}
	finding.Problem = strings.TrimSpace(strings.TrimPrefix(strings.Join(body[:fixAt], "\n"), renderedProblem))
	finding.Fix = strings.TrimSpace(strings.TrimPrefix(strings.Join(body[fixAt:], "\n"), renderedFix))
	if finding.Problem == "" {
		return Finding{}, refuse(RefusalMissingProblem, "finding %d has an empty Problem field", number)
	}
	if finding.Fix == "" {
		return Finding{}, refuse(RefusalMissingFix, "finding %d has an empty Fix field", number)
	}
	return finding, nil
}

// sourcesFromLabels maps a rendered "Reported by" list back to review numbers.
// Rendering drops duplicate labels, so a label shared by two input reviews
// cannot be attributed and the finding is refused.
func sourcesFromLabels(number int, rendered string, sourceLabels []string) ([]int, *MarkdownRefusal) {
	var sources []int
	for label := range strings.SplitSeq(rendered, ", ") {
		match := 0
		for i, candidate := range sourceLabels {
			if candidate != label {
				continue
			}
			if match != 0 {
				return nil, refuse(RefusalUnrecoverableSources,
					"finding %d names a reviewer label shared by more than one input review", number)
			}
			match = i + 1
		}
		if match == 0 {
			return nil, refuse(RefusalUnrecoverableSources,
				"finding %d names a reviewer that is not one of the input reviews", number)
		}
		sources = append(sources, match)
	}
	return sources, nil
}

func verdictFromLabel(label string) (string, bool) {
	for _, verdict := range []string{VerdictPass, VerdictFail, VerdictUnableToReview} {
		if verdictLabel(verdict) == label {
			return verdict, true
		}
	}
	return "", false
}

// legacyFinding collects one bullet group. A nil field was never stated.
type legacyFinding struct {
	severity, location, problem, fix *string
}

func (f *legacyFinding) field(label string) **string {
	switch label {
	case "Severity":
		return &f.severity
	case "Location":
		return &f.location
	case "Problem":
		return &f.problem
	default:
		return &f.fix
	}
}

// parseFindingsList reads the findings list older review prompts asked for.
// Every line must be a field bullet, an indented continuation of one, a
// separator, or part of the Summary section. Anything else is refused.
func parseFindingsList(lines []string) (Document, *MarkdownRefusal) {
	var groups []*legacyFinding
	var current *legacyFinding
	var value *string
	pendingBlank := false
	summaryAt := -1
	for i, line := range lines[1:] {
		if legacySummaryHeading.MatchString(line) {
			summaryAt = i + 2
			break
		}
		switch {
		case strings.TrimSpace(line) == "":
			pendingBlank = value != nil
		case legacySeparator.MatchString(line):
			current, value, pendingBlank = nil, nil, false
		case legacyFieldBullet.MatchString(line):
			m := legacyFieldBullet.FindStringSubmatch(line)
			if m[1] == "Severity" || current == nil {
				current = &legacyFinding{}
				groups = append(groups, current)
			}
			slot := current.field(m[1])
			if *slot != nil {
				return Document{}, refuse(RefusalUnrecognizedContent,
					"finding %d states %s twice", len(groups), m[1])
			}
			text := strings.TrimSpace(m[2])
			*slot, value, pendingBlank = &text, &text, false
		case value != nil && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")):
			if pendingBlank {
				*value += "\n"
			}
			*value += "\n" + strings.TrimPrefix(strings.TrimPrefix(line, "\t"), "  ")
			pendingBlank = false
		default:
			return Document{}, refuse(RefusalUnrecognizedContent,
				"line %d of the findings list is not a finding field", i+2)
		}
	}
	if summaryAt < 0 {
		return Document{}, refuse(RefusalMissingSummary, "the review has no Summary section")
	}
	summaryLines := lines[summaryAt:]
	for _, line := range summaryLines {
		if markdownHeading.MatchString(line) || legacyFieldBullet.MatchString(line) {
			return Document{}, refuse(RefusalUnrecognizedContent, "the Summary section holds more review structure")
		}
	}
	doc := Document{SchemaVersion: 1, Findings: []Finding{}}
	doc.Summary = strings.TrimSpace(strings.Join(summaryLines, "\n"))
	if doc.Summary == "" {
		return Document{}, refuse(RefusalMissingSummary, "the Summary section is empty")
	}
	if len(groups) == 0 {
		return Document{}, refuse(RefusalUnrecognizedContent, "a Review Findings heading without findings")
	}
	for i, group := range groups {
		finding, refusal := group.finding(i + 1)
		if refusal != nil {
			return Document{}, refusal
		}
		doc.Findings = append(doc.Findings, finding)
	}
	return doc, nil
}

func (f *legacyFinding) finding(number int) (Finding, *MarkdownRefusal) {
	if f.severity == nil || strings.TrimSpace(*f.severity) == "" {
		return Finding{}, refuse(RefusalMissingSeverity, "finding %d has no severity", number)
	}
	severity := strings.ToLower(strings.TrimSpace(*f.severity))
	if severityRank(severity) == 0 {
		return Finding{}, refuse(RefusalInvalidSeverity,
			"finding %d has a severity other than critical, high, medium, or low", number)
	}
	if f.problem == nil || strings.TrimSpace(*f.problem) == "" {
		return Finding{}, refuse(RefusalMissingProblem, "finding %d has no problem", number)
	}
	if f.fix == nil || strings.TrimSpace(*f.fix) == "" {
		return Finding{}, refuse(RefusalMissingFix, "finding %d has no fix", number)
	}
	finding := Finding{Severity: severity, Problem: strings.TrimSpace(*f.problem), Fix: strings.TrimSpace(*f.fix)}
	if f.location != nil {
		finding.Location = strings.TrimSpace(*f.location)
	}
	return finding, nil
}

// parseNoIssues reads a review that states "No issues found." as its first or
// last line. All other text becomes the summary, so nothing is dropped. Text
// with headings or finding fields is some other structure and is refused.
func parseNoIssues(lines []string) (Document, *MarkdownRefusal) {
	first, last := noIssuesLine.MatchString(strings.TrimSpace(lines[0])), noIssuesLine.MatchString(strings.TrimSpace(lines[len(lines)-1]))
	var rest []string
	switch {
	case first:
		rest = lines[1:]
	case last:
		rest = lines[:len(lines)-1]
	default:
		return Document{}, refuse(RefusalUnrecognizedFormat, "the text is not in a review format roborev wrote")
	}
	for _, line := range rest {
		if markdownHeading.MatchString(line) || legacyFieldBullet.MatchString(line) || noIssuesLine.MatchString(strings.TrimSpace(line)) {
			return Document{}, refuse(RefusalUnrecognizedContent,
				"the text around \"No issues found.\" holds more review structure")
		}
	}
	summary := strings.TrimSpace(strings.Join(rest, "\n"))
	summary = strings.TrimSpace(summaryLabel.ReplaceAllString(summary, ""))
	if summary == "" {
		summary = renderedNoIssues
	}
	return Document{SchemaVersion: 1, Summary: summary, Findings: []Finding{}}, nil
}

func trimBlankLines(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
