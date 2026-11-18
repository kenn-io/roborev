package storage

import (
	"encoding/json/v2"
	"strings"

	"go.kenn.io/roborev/pkg/structuredreview"
)

// CompactAuditPrefix identifies canonical summaries produced by accountable
// compact. It survives storage/export without a new public document field.
const CompactPromptPrefix = "# roborev compact decisions v1\n"

const CompactAuditPrefix = "<!-- roborev:compact-audit:v1 -->\n"

// FixReviewContext removes historical compact decisions and agent commentary
// only for identified canonical compacts. Presentation/export keep the full
// audit; ordinary/custom review outputs retain their existing behavior.
func FixReviewContext(review *Review) string {
	if review == nil {
		return ""
	}
	if review.Job == nil || review.Job.JobType != JobTypeCompact || !strings.HasPrefix(review.Prompt, CompactPromptPrefix) {
		return review.Output
	}
	if len(review.StructuredOutput) == 0 {
		return review.Output
	}
	raw, err := json.Marshal(review.StructuredOutput)
	if err != nil {
		return review.Output
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil || !strings.HasPrefix(doc.Summary, CompactAuditPrefix) {
		return review.Output
	}
	doc.Summary = "Retained compact findings to address. Historical decisions are omitted from fix context."
	return doc.Markdown("")
}
