package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
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
