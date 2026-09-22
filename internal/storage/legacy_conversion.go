package storage

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// Refusal reasons the storage layer adds to the Markdown parser's own. Together
// they are the keys of LegacyConversionReport.Refused.
const (
	// LegacyRefusalNotAReview marks output that never reviewed the code, such
	// as empty output or an agent that could not read its diff.
	LegacyRefusalNotAReview = "not_a_review"
	// LegacyRefusalVerdictMismatch marks a conversion whose findings would
	// change the verdict roborev already recorded for the review.
	LegacyRefusalVerdictMismatch = "verdict_mismatch"
	// LegacyRefusalActiveReviewExists marks an archived review whose job
	// already has an active review, for example after a rerun.
	LegacyRefusalActiveReviewExists = "active_review_exists"
	// LegacyRefusalJobMissing marks an archived review whose job was deleted,
	// so it has nowhere to be restored to.
	LegacyRefusalJobMissing = "job_missing"
)

// legacyThresholdSummary is the summary of a review whose whole output was
// config.SeverityThresholdMarker. It restates the marker's defined meaning and
// says nothing about the code, because the reviewer recorded nothing else.
const legacyThresholdSummary = "The reviewer reported " + config.SeverityThresholdMarker +
	": every finding was below the configured minimum severity, and none was recorded."

// legacyMarkdown is an archived or not yet migrated Markdown review, with the
// stored facts a conversion must agree with.
type legacyMarkdown struct {
	Markdown    string
	JobType     string
	MinSeverity string
	// StoredVerdict is the verdict roborev recorded, or nil when none was.
	StoredVerdict *bool
	// SourceLabels names a synthesis review's input reviews in number order.
	SourceLabels []string
}

// legacyRefusal says why a Markdown review was left for manual conversion.
type legacyRefusal struct {
	Reason string
	Detail string
}

// convertLegacyMarkdown builds the JSON document for a Markdown review that
// roborev itself wrote. It refuses instead of guessing: the parser only reads
// stated fields, a synthesis review must keep recoverable sources, and the
// converted findings must produce the verdict roborev already recorded.
func convertLegacyMarkdown(in legacyMarkdown) (jsontext.Value, *legacyRefusal) {
	if ClassifyOutput(in.Markdown) != OutputReviewed {
		return nil, &legacyRefusal{LegacyRefusalNotAReview, "the output is not a review"}
	}
	var doc structuredreview.Document
	if config.IsMarkerOnlyOutput(in.Markdown) {
		doc = structuredreview.Document{SchemaVersion: 1, Summary: legacyThresholdSummary, Findings: []structuredreview.Finding{}}
	} else {
		parsed, err := structuredreview.ParseMarkdown(in.Markdown, structuredreview.ParseMarkdownOptions{SourceLabels: in.SourceLabels})
		if err != nil {
			if refusal, ok := errors.AsType[*structuredreview.MarkdownRefusal](err); ok {
				return nil, &legacyRefusal{string(refusal.Reason), refusal.Detail}
			}
			return nil, &legacyRefusal{string(structuredreview.RefusalUnrecognizedFormat), err.Error()}
		}
		doc = parsed
	}
	if in.JobType == JobTypeSynthesis {
		doc.SourceLabels = in.SourceLabels
		if err := doc.RequireSources(len(doc.SourceLabels)); err != nil {
			return nil, &legacyRefusal{string(structuredreview.RefusalUnrecoverableSources), err.Error()}
		}
	}

	expected := in.StoredVerdict
	if expected == nil {
		if verdict := ParseVerdictAtSeverity(in.Markdown, in.MinSeverity); verdict != VerdictUnknown {
			expected = new(verdict.Passed())
		}
	}
	if expected != nil && (doc.UnableToReview() || doc.Passed(in.MinSeverity) != *expected) {
		return nil, &legacyRefusal{
			LegacyRefusalVerdictMismatch,
			"the converted findings would change the verdict recorded for this review",
		}
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, &legacyRefusal{string(structuredreview.RefusalUnrecognizedContent), err.Error()}
	}
	// The import path decodes the document again. Decode here too so a
	// document it would reject counts as a refusal, not an import error.
	if _, err := structuredreview.Decode(raw); err != nil {
		return nil, &legacyRefusal{string(structuredreview.RefusalUnrecognizedContent), err.Error()}
	}
	return raw, nil
}

// convertLegacyRecord prefers an existing document over reparsing its rendered
// Markdown. Older synthesis documents stored source numbers without labels;
// reconstruct labels from their recorded panel members before validating them.
func convertLegacyRecord(in legacyMarkdown, previousJSON string) (jsontext.Value, *legacyRefusal) {
	raw := jsontext.Value(previousJSON)
	if len(raw) == 0 {
		raw = jsontext.Value(in.Markdown)
	}
	if doc, err := structuredreview.Decode(raw); err == nil && doc.Legacy == nil {
		if in.JobType != JobTypeSynthesis || doc.RequireSources(len(doc.SourceLabels)) == nil {
			return raw, nil
		}
		if len(doc.SourceLabels) == 0 {
			doc.SourceLabels = in.SourceLabels
			if doc.RequireSources(len(doc.SourceLabels)) == nil {
				converted, err := json.Marshal(doc)
				if err == nil {
					return converted, nil
				}
			}
		}
	}
	return convertLegacyMarkdown(in)
}

func legacySourceLabels(sources []LegacyReviewSource) []string {
	labels := make([]string, 0, len(sources))
	for _, source := range sources {
		labels = append(labels, source.Agent)
	}
	return labels
}

// LegacyConversionReport counts what an automatic conversion did, or would do
// in a dry run.
type LegacyConversionReport struct {
	// Unresolved is the number of archived reviews that needed conversion.
	Unresolved int `json:"unresolved"`
	// Converted is the number restored, or that a dry run would restore.
	Converted int `json:"converted"`
	// Refused counts the reviews left archived, by refusal reason.
	Refused map[string]int `json:"refused"`
	DryRun  bool           `json:"dry_run"`
}

func newLegacyConversionReport(dryRun bool) LegacyConversionReport {
	return LegacyConversionReport{Refused: map[string]int{}, DryRun: dryRun}
}

// RefusalReasons returns the refusal reasons in a stable order for display.
func (r LegacyConversionReport) RefusalReasons() []string {
	return slices.Sorted(maps.Keys(r.Refused))
}

// ConvertLegacyReviews converts every unresolved archived review that roborev
// itself wrote in a recognized Markdown format, and imports each through
// ResolveLegacyReview, so validation and archiving match a manual import.
// Reviews it refuses stay archived and unresolved. A second run finds nothing
// new to convert. A dry run changes nothing.
func (db *DB) ConvertLegacyReviews(dryRun bool) (LegacyConversionReport, error) {
	report := newLegacyConversionReport(dryRun)
	records, err := db.UnresolvedLegacyReviews()
	if err != nil {
		return report, err
	}
	report.Unresolved = len(records)
	for _, record := range records {
		if record.JobType == "" {
			report.Refused[LegacyRefusalJobMissing]++
			continue
		}
		var active bool
		if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM reviews WHERE (job_id = ? OR uuid = ?) AND (uuid IS NOT ? OR json_extract(structured_output, '$.legacy') IS NULL))`,
			record.JobID, record.uuid, record.uuid).Scan(&active); err != nil {
			return report, err
		}
		if active {
			report.Refused[LegacyRefusalActiveReviewExists]++
			continue
		}
		raw, refusal := convertLegacyRecord(legacyMarkdown{
			Markdown: record.Output, JobType: record.JobType, MinSeverity: record.minSeverity,
			StoredVerdict: record.storedVerdict, SourceLabels: legacySourceLabels(record.Sources),
		}, record.StructuredOutput)
		if refusal != nil {
			report.Refused[refusal.Reason]++
			continue
		}
		if !dryRun {
			if err := db.ResolveLegacyReview(record.ID, raw); err != nil {
				return report, err
			}
		}
		report.Converted++
	}
	return report, nil
}
