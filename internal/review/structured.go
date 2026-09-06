package review

import (
	"encoding/json"
	"fmt"

	"go.kenn.io/roborev/internal/structuredreview"
)

var CustomReviewSchema = structuredreview.Schema

type StructuredReview = structuredreview.Document

type StructuredFinding = structuredreview.Finding

func DecodeStructuredReview(raw json.RawMessage) (StructuredReview, error) {
	return structuredreview.Decode(raw)
}

// validateLiveDocument rejects documents a live agent must not return. Stored
// documents may be older versions, but a live agent must honor the schema it
// was given: an older shape means the provider ignored it. A failing verdict
// with no finding says nothing actionable, and unable_to_review means the
// agent never assessed the change; both are agent errors so retry and backup
// failover run instead of recording a misleading review.
func validateLiveDocument(agentName string, doc StructuredReview) error {
	if doc.SchemaVersion != structuredreview.SchemaVersion {
		return fmt.Errorf(
			"agent %s returned structured review schema_version %d, want %d",
			agentName, doc.SchemaVersion, structuredreview.SchemaVersion,
		)
	}
	if doc.Verdict == structuredreview.VerdictFail && len(doc.Findings) == 0 {
		return fmt.Errorf(
			"agent %s reported a failing verdict without any finding: %s",
			agentName, doc.Summary,
		)
	}
	if doc.UnableToReview() {
		return fmt.Errorf(
			"agent %s could not review the change: %s",
			agentName, doc.Summary,
		)
	}
	return nil
}
