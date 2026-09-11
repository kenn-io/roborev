package storage

import (
	"encoding/json"

	"go.kenn.io/roborev/internal/structuredreview"
)

// completeReviewFixture creates a JSON review for tests whose subject is job
// lifecycle or metadata rather than agent output parsing. Tests of findings
// should supply their own structured document through CompleteJobResult.
func completeReviewFixture(db *DB, id int64, agent, prompt, summary string) error {
	job, err := db.GetJobByID(id)
	if err != nil {
		return err
	}
	if job.IsTaskJob() || job.IsFixJob() {
		return db.CompleteJob(id, agent, prompt, summary)
	}
	raw := reviewFixtureJSON(summary)
	if job.JobType == "synthesis" {
		doc, err := structuredreview.Decode(raw)
		if err != nil {
			return err
		}
		doc.SourceLabels = []string{agent}
		for i := range doc.Findings {
			doc.Findings[i].Sources = []int{1}
		}
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
	}

	return db.CompleteJobResult(id, agent, prompt, ReviewCompletion{StructuredOutput: raw})
}

// reviewFixtureJSON describes a synthetic fixture, not a legacy conversion.
func reviewFixtureJSON(summary string) json.RawMessage {
	if json.Valid([]byte(summary)) {
		return json.RawMessage(summary)
	}
	doc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion, Summary: summary,
		Verdict: structuredreview.VerdictPass, Findings: []structuredreview.Finding{},
	}
	if doc.Summary == "" {
		doc.Summary = "No review output was produced."
	}
	switch ParseVerdict(summary) {
	case VerdictFail:
		severity := HighestSeverityLabel(summary)
		if severity == "" {
			severity = "medium"
		}
		doc.Verdict = structuredreview.VerdictFail
		doc.Findings = []structuredreview.Finding{{Severity: severity, Problem: summary, Fix: "Apply the correction described in the test finding."}}
	case VerdictUnknown:
		doc.Verdict = structuredreview.VerdictUnableToReview
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}
