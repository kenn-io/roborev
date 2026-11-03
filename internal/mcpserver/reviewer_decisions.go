package mcpserver

import (
	"regexp"
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
}

// fixCommentLine matches the disposition lines the roborev-fix skill writes:
// "- <severity> <file>: <Fix|Decline|Invalid|Ask>: <reason>".
var fixCommentLine = regexp.MustCompile(
	`(?i)^\s*[-*]\s*(critical|high|medium|low)\s+(.+?):\s*(fix|decline|invalid|ask)\s*:\s*(.*\S)\s*$`,
)

var trailingLines = regexp.MustCompile(`:\d+(-\d+)?$`)

type parsedDecision struct {
	severity string
	file     string
	decision reviewerDecision
}

// parseFixComment returns the disposition lines of a comment and whether
// every non-empty line was one, in which case decisions carry it in full.
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
		parsed = append(parsed, parsedDecision{
			severity: strings.ToLower(m[1]),
			file:     trailingLines.ReplaceAllString(strings.Trim(m[2], "`"), ""),
			decision: reviewerDecision{
				Kind: strings.ToLower(m[3]), Reason: m[4], Source: DecisionSourceFixComment, By: comment.Responder,
			},
		})
	}
	return parsed, complete && len(parsed) > 0
}

// findingFile is the file a finding names. Version 3 documents carry it
// directly; older ones only have a display location such as "path:12".
func findingFile(f structuredreview.Finding) string {
	if f.File != "" {
		return f.File
	}
	return trailingLines.ReplaceAllString(f.Location, "")
}

// attachDecisions files each decision under the finding it answers. A fix
// comment line names a severity and file; when several findings share both,
// lines answer them in order, as the fix skill writes one line per finding.
// An assessment names the finding by an excerpt of its text. Decisions that
// match no finding stay on the review. It returns the comments that still
// hold text the decisions do not carry.
func attachDecisions(
	findings []reviewerFinding, doc structuredreview.Document, review BranchReview,
) (unmatched []reviewerDecision, comments []storage.Response) {
	answered := make([]bool, len(findings))
	for _, comment := range review.Comments {
		parsed, complete := parseFixComment(comment)
		if !complete {
			comments = append(comments, comment)
		}
		for _, p := range parsed {
			i := matchFixLine(doc.Findings, answered, p)
			if i < 0 {
				unmatched = append(unmatched, p.decision)
				continue
			}
			answered[i] = true
			findings[i].Decisions = append(findings[i].Decisions, p.decision)
		}
	}
	for _, a := range review.Assessments {
		decision := reviewerDecision{Kind: a.Outcome, Reason: a.Explanation, Source: DecisionSourceRebuttal}
		if i := matchExcerpt(doc.Findings, a.Finding); i >= 0 {
			findings[i].Decisions = append(findings[i].Decisions, decision)
		} else {
			unmatched = append(unmatched, decision)
		}
	}
	return unmatched, comments
}

func matchFixLine(findings []structuredreview.Finding, answered []bool, p parsedDecision) int {
	first := -1
	for i, f := range findings {
		if strings.ToLower(f.Severity) != p.severity || findingFile(f) != p.file {
			continue
		}
		if !answered[i] {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	return first
}

func matchExcerpt(findings []structuredreview.Finding, excerpt string) int {
	needle := normalizeSpace(excerpt)
	if needle == "" {
		return -1
	}
	for i, f := range findings {
		if strings.Contains(normalizeSpace(f.Problem), needle) || strings.Contains(normalizeSpace(f.Fix), needle) {
			return i
		}
	}
	return -1
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
