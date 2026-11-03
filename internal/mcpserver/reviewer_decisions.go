package mcpserver

import (
	"regexp"
	"strings"

	"go.kenn.io/roborev/internal/storage"
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
	BeforeRerun bool // queued before the review's current run was written
}

type reviewerDecision struct {
	Kind        string `json:"kind" jsonschema:"fix, decline, invalid, or ask from a fix comment; confirmed, refuted, or superseded from a rebuttal re-check"`
	Target      string `json:"target" jsonschema:"the finding as the decision named it: severity and file, or an excerpt of the review"`
	Reason      string `json:"reason"`
	Source      string `json:"source" jsonschema:"fix_comment or rebuttal"`
	By          string `json:"by,omitempty"`
	BeforeRerun bool   `json:"before_rerun,omitempty" jsonschema:"true when the decision was recorded on an earlier run of this review, whose findings may differ"`
}

// fixCommentLine matches the disposition lines the roborev-fix skill writes:
// "- <severity> <file>: <Fix|Decline|Invalid|Ask>: <reason>".
var fixCommentLine = regexp.MustCompile(
	`(?i)^\s*[-*]\s*(critical|high|medium|low)\s+(.+?):\s*(fix|decline|invalid|ask)\s*:\s*(.*\S)\s*$`,
)

// reviewDecisions turns a review's disposition comments and rebuttal
// re-checks into typed decisions. A decision names its finding only by what
// its author wrote, so the decisions stay on the review and the reviewer
// matches them to findings. It returns the comments that still hold text the
// decisions do not carry.
func reviewDecisions(review BranchReview) (decisions []reviewerDecision, comments []storage.Response) {
	for _, comment := range review.Comments {
		// A rerun keeps the job's comments but replaces its findings.
		beforeRerun := comment.CreatedAt.Before(review.Review.CreatedAt)
		complete := true
		parsed := 0
		for line := range strings.Lines(comment.Response) {
			if strings.TrimSpace(line) == "" {
				continue
			}
			m := fixCommentLine.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
			if m == nil {
				complete = false
				continue
			}
			parsed++
			decisions = append(decisions, reviewerDecision{
				Kind:        strings.ToLower(m[3]),
				Target:      strings.ToLower(m[1]) + " " + strings.Trim(m[2], "`"),
				Reason:      m[4],
				Source:      DecisionSourceFixComment,
				By:          comment.Responder,
				BeforeRerun: beforeRerun,
			})
		}
		if !complete || parsed == 0 {
			comments = append(comments, comment)
		}
	}
	for _, a := range review.Assessments {
		decisions = append(decisions, reviewerDecision{
			Kind: a.Outcome, Target: a.Finding, Reason: a.Explanation, Source: DecisionSourceRebuttal,
			BeforeRerun: a.BeforeRerun,
		})
	}
	return decisions, comments
}
