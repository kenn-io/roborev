package review

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/roborev/pkg/structuredreview"
)

var CustomReviewSchema = structuredreview.Schema

type StructuredReview = structuredreview.Document

type StructuredFinding = structuredreview.Finding

func DecodeStructuredReview(raw jsontext.Value) (StructuredReview, error) {
	return structuredreview.Decode(raw)
}

// validateLiveDocument rejects documents a live agent must not return. Stored
// documents may be older versions, or come from producers without a verdict,
// kind, or confidence, but a live agent must honor the schema it was given: an
// older shape or a missing field means the provider ignored it. A failing verdict
// with no finding says nothing actionable, and unable_to_review means the
// agent never assessed the change; both are agent errors so retry and backup
// failover run instead of recording a misleading review.
func validateLiveDocument(agentName string, doc StructuredReview, raw jsontext.Value) error {
	if doc.SchemaVersion != structuredreview.SchemaVersion {
		return fmt.Errorf(
			"agent %s returned structured review schema_version %d, want %d",
			agentName, doc.SchemaVersion, structuredreview.SchemaVersion,
		)
	}
	if doc.Verdict == "" {
		return fmt.Errorf("agent %s returned a structured review without a verdict", agentName)
	}
	for i, finding := range doc.Findings {
		if finding.Kind == "" {
			return fmt.Errorf(
				"agent %s returned structured review finding %d without a kind or confidence",
				agentName, i+1,
			)
		}
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
	return validateLiveClassifications(agentName, raw)
}

func validateLiveClassifications(agentName string, raw jsontext.Value) error {
	var document struct {
		Findings []struct {
			Nit      *bool `json:"nit"`
			Unlikely *bool `json:"unlikely"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return fmt.Errorf("decode agent %s classifications: %w", agentName, err)
	}
	for i, finding := range document.Findings {
		if finding.Nit == nil || finding.Unlikely == nil {
			return fmt.Errorf("agent %s returned structured review finding %d without nit or unlikely", agentName, i+1)
		}
	}
	return nil
}
