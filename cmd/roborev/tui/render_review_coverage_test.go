package tui

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestReviewDetailsRenderStoredFileCoverage(t *testing.T) {
	zero, excluded := 0, 3
	job := makeJob(42)
	review := makeReview(1, &job, withReviewOutput("review output"))
	review.FileCoverage = &storage.ReviewFileCoverage{Reviewed: &zero, Excluded: &excluded}
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.currentReview = review

	full := stripANSI(m.renderReviewView())
	assert.Contains(t, full, "0 files reviewed, 3 excluded")
	header := m.reviewPaneHeaderLines(120)
	require.Len(t, header, 3)
	assert.Contains(t, stripANSI(header[1]), "0 files reviewed, 3 excluded")
	withCoverageLines := strings.Count(full, "\n")

	review.FileCoverage = nil
	without := stripANSI(m.renderReviewView())
	assert.Equal(t, strings.Count(without, "\n"), withCoverageLines)
	assert.NotContains(t, without, "files reviewed")
}

func TestReviewDetailsRenderLegacyDocument(t *testing.T) {
	doc, err := structuredreview.Decode(jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"## Historical finding\n\nThe write loses data.","recorded_verdict":false}}`))
	require.NoError(t, err)
	job := makeJob(42)
	review := makeReview(1, &job, withReviewOutput(doc.Markdown("")))
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width, m.height = 120, 30
	m.currentReview = review
	full := stripANSI(m.renderReviewView())
	assert.Contains(t, full, "Unstructured historical review")
	assert.Contains(t, full, "The write loses data.")
	assert.NotContains(t, full, "No issues found")
	assert.Equal(t, "-", findingCountsCell(job.FindingCounts))
}
