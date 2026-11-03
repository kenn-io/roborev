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
	severity   string
	file       string
	start, end int // 0 when the line named no line numbers
	target     string
	decision   reviewerDecision
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
		file, start, end := splitLocation(where)
		parsed = append(parsed, parsedDecision{
			severity: strings.ToLower(m[1]),
			file:     file,
			start:    start,
			end:      end,
			target:   strings.ToLower(m[1]) + " " + where,
			decision: reviewerDecision{
				Kind: strings.ToLower(m[3]), Reason: m[4], Source: DecisionSourceFixComment, By: comment.Responder,
			},
		})
	}
	return parsed, complete && len(parsed) > 0
}

// splitLocation splits "path", "path:12", or "path:12-20" into the path and
// its line range; a single line has start == end.
func splitLocation(location string) (file string, start, end int) {
	m := trailingLines.FindStringSubmatch(location)
	if m == nil {
		return location, 0, 0
	}
	start, _ = strconv.Atoi(m[1])
	end = start
	if m[2] != "" {
		end, _ = strconv.Atoi(m[2])
	}
	return strings.TrimSuffix(location, m[0]), start, end
}

// findingSpan is the file and line range a finding names. Version 3
// documents carry them directly; older ones only have a display location.
func findingSpan(f structuredreview.Finding) (file string, start, end int) {
	if f.File != "" {
		return f.File, f.StartLine, max(f.StartLine, f.EndLine)
	}
	return splitLocation(f.Location)
}

// attachDecisions files each decision under the finding it unambiguously
// answers and keeps the rest on the review, with the target they named.
//
//   - A fix comment line matches the findings with its severity and file,
//     narrowed by its line range when it has one. Only a single remaining
//     finding that no earlier line of the same comment answered is a match.
//   - A comment on the commit rather than the job reaches every review of
//     that commit, and a comment older than the current review answered an
//     earlier run of it, so neither attaches to this review's findings.
//   - An assessment matches the one finding whose rendered text holds its
//     excerpt.
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
			if comment.JobID != nil && !comment.CreatedAt.Before(review.Review.CreatedAt) {
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
		if i := matchExcerpt(doc, a.Finding); i >= 0 {
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
		if p.start > 0 && start > 0 && (p.end < start || p.start > end) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

// matchExcerpt returns the 0-based finding whose rendered text holds
// excerpt, or -1 unless exactly one does. Rebuttals accept only excerpts of
// the rendered review, so each finding is rendered alone by the same writer,
// labels included.
func matchExcerpt(doc structuredreview.Document, excerpt string) int {
	needle := normalizeSpace(excerpt)
	if needle == "" {
		return -1
	}
	match := -1
	for i, f := range doc.Findings {
		alone := structuredreview.Document{SchemaVersion: doc.SchemaVersion, Findings: []structuredreview.Finding{f}}
		if !strings.Contains(normalizeSpace(alone.Markdown("")), needle) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = i
	}
	return match
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
