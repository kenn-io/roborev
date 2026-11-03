package mcpserver

import (
	"regexp"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// Decision sources. Later typed sources, such as Kata finding replies, add
// their own value instead of a new field.
const (
	DecisionSourceFixComment = "fix_comment"
	DecisionSourceRebuttal   = "rebuttal"
)

// FindingAssessment is a rebuttal re-check of one finding of a review. Finding
// is the excerpt the developer selected from the review text.
type FindingAssessment struct {
	Finding     string
	Outcome     string
	Explanation string
}

type reviewerDecision struct {
	Kind   string `json:"kind" jsonschema:"fix, decline, invalid, or ask from a fix comment; confirmed, refuted, or superseded from a rebuttal re-check"`
	Reason string `json:"reason"`
	Source string `json:"source" jsonschema:"fix_comment or rebuttal"`
	By     string `json:"by,omitempty"`
	Target string `json:"target,omitempty" jsonschema:"for a decision on the review rather than one finding: the severity and file or the excerpt it named"`
}

// fixCommentLine matches the disposition lines the roborev-fix skill writes:
// "- <severity> <file>: <Fix|Decline|Invalid|Ask>: <reason>". Agents often
// write the file as file:line, which narrows the match.
var fixCommentLine = regexp.MustCompile(
	`(?i)^\s*[-*]\s*(critical|high|medium|low)\s+(.+?):\s*(fix|decline|invalid|ask)\s*:\s*(.*\S)\s*$`,
)

var trailingLines = regexp.MustCompile(`:(\d+)(?:-(\d+))?$`)

type parsedDecision struct {
	severity string
	file     string
	line     int // 0 when the line named no line number
	target   string
	decision reviewerDecision
}

// parseFixComment returns the disposition lines of a comment and whether
// every non-empty line was one.
func parseFixComment(comment storage.Response) ([]parsedDecision, bool) {
	var parsed []parsedDecision
	complete := true
	for line := range strings.Lines(comment.Response) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := fixCommentLine.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
		if m == nil {
			complete = false
			continue
		}
		where := strings.Trim(m[2], "`")
		file, lineNo := splitLocation(where)
		parsed = append(parsed, parsedDecision{
			severity: strings.ToLower(m[1]),
			file:     file,
			line:     lineNo,
			target:   strings.ToLower(m[1]) + " " + where,
			decision: reviewerDecision{
				Kind: strings.ToLower(m[3]), Reason: m[4], Source: DecisionSourceFixComment, By: comment.Responder,
			},
		})
	}
	return parsed, complete && len(parsed) > 0
}

// splitLocation splits "path:12" or "path:12-20" into the path and its first
// line.
func splitLocation(location string) (string, int) {
	m := trailingLines.FindStringSubmatchIndex(location)
	if m == nil {
		return location, 0
	}
	line, _ := strconv.Atoi(location[m[2]:m[3]])
	return location[:m[0]], line
}

// findingSpan is the file and line range a finding names. Version 3
// documents carry them directly; older ones only have a display location.
func findingSpan(f structuredreview.Finding) (file string, start, end int) {
	if f.File != "" {
		return f.File, f.StartLine, f.EndLine
	}
	file, start = splitLocation(f.Location)
	if m := trailingLines.FindStringSubmatch(f.Location); m != nil && m[2] != "" {
		end, _ = strconv.Atoi(m[2])
	}
	return file, start, end
}

// attachDecisions files each decision under the finding it unambiguously
// answers and keeps the rest on the review, with the target they named.
//
//   - A fix comment line matches the findings with its severity and file,
//     narrowed by its line number when it has one. Only a single remaining
//     finding that no earlier line of the same comment answered is a match.
//   - A comment on the commit rather than the job reaches every review of
//     that commit, so its lines never attach to one review's findings.
//   - An assessment matches the finding whose rendered section holds its
//     excerpt, when the excerpt appears in exactly one section.
//
// It returns the comments that still hold text the decisions do not carry.
func attachDecisions(
	findings []reviewerFinding, doc structuredreview.Document, review BranchReview,
) (unmatched []reviewerDecision, comments []storage.Response) {
	for _, comment := range review.Comments {
		parsed, complete := parseFixComment(comment)
		if !complete {
			comments = append(comments, comment)
		}
		answered := make([]bool, len(findings))
		for _, p := range parsed {
			i := -1
			if comment.JobID != nil {
				i = matchFixLine(doc.Findings, answered, p)
			}
			if i < 0 {
				d := p.decision
				d.Target = p.target
				unmatched = append(unmatched, d)
				continue
			}
			answered[i] = true
			findings[i].Decisions = append(findings[i].Decisions, p.decision)
		}
	}
	for _, a := range review.Assessments {
		decision := reviewerDecision{Kind: a.Outcome, Reason: a.Explanation, Source: DecisionSourceRebuttal}
		if i := matchExcerpt(review.Review.Output, len(findings), a.Finding); i >= 0 {
			findings[i].Decisions = append(findings[i].Decisions, decision)
		} else {
			decision.Target = a.Finding
			unmatched = append(unmatched, decision)
		}
	}
	return unmatched, comments
}

func matchFixLine(findings []structuredreview.Finding, answered []bool, p parsedDecision) int {
	match := -1
	for i, f := range findings {
		file, start, end := findingSpan(f)
		if strings.ToLower(f.Severity) != p.severity || file != p.file || answered[i] {
			continue
		}
		if p.line > 0 && start > 0 && (p.line < start || p.line > max(start, end)) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

var findingHeader = regexp.MustCompile(`### (\d+)\. `)

// matchExcerpt returns the 0-based finding whose rendered section in output
// holds excerpt, or -1 unless every occurrence falls in that one section.
// Rebuttals accept only excerpts of the rendered review, and the renderer
// numbers each finding's section "### <index>. <Severity>".
func matchExcerpt(output string, count int, excerpt string) int {
	text, needle := normalizeSpace(output), normalizeSpace(excerpt)
	if needle == "" {
		return -1
	}
	headers := findingHeader.FindAllStringSubmatchIndex(text, -1)
	match := -1
	for from := 0; ; {
		at := strings.Index(text[from:], needle)
		if at < 0 {
			break
		}
		at += from
		section := -1
		for _, h := range headers {
			if h[0] > at {
				break
			}
			section, _ = strconv.Atoi(text[h[2]:h[3]])
			section--
		}
		// An excerpt before the first section, or across two, names no finding.
		next := len(text)
		for _, h := range headers {
			if h[0] > at {
				next = h[0]
				break
			}
		}
		if section < 0 || section >= count || at+len(needle) > next || (match >= 0 && match != section) {
			return -1
		}
		match = section
		from = at + 1
	}
	return match
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
