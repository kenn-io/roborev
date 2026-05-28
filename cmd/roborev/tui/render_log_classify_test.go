package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/muesli/ansi"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestClassifyReasoningLines(t *testing.T) {
	tests := []struct {
		name      string
		job       *storage.ReviewJob
		want      []string
		notWant   []string
		wantEmpty bool
	}{
		{
			name:      "nil job",
			job:       nil,
			wantEmpty: true,
		},
		{
			name: "plain review job",
			job: &storage.ReviewJob{
				ID:      1,
				JobType: storage.JobTypeReview,
				Status:  storage.JobStatusDone,
			},
			wantEmpty: true,
		},
		{
			name: "running classify",
			job: &storage.ReviewJob{
				ID:      2,
				JobType: storage.JobTypeClassify,
				Status:  storage.JobStatusRunning,
				Source:  "auto_design",
			},
			want: []string{"in progress"},
		},
		{
			name: "queued classify",
			job: &storage.ReviewJob{
				ID:      20,
				JobType: storage.JobTypeClassify,
				Status:  storage.JobStatusQueued,
				Source:  "auto_design",
			},
			want: []string{"in progress"},
		},
		{
			// Terminal classify failures must not claim "in progress";
			// the operator pressing 'l' needs the actual status.
			name: "failed classify",
			job: &storage.ReviewJob{
				ID:      21,
				JobType: storage.JobTypeClassify,
				Status:  storage.JobStatusFailed,
				Source:  "auto_design",
				Error:   "exec: classifier: timeout after 30s",
			},
			want: []string{"failed", "timeout"},
			notWant: []string{
				"in progress",
				"no design review needed",
			},
		},
		{
			name: "canceled classify",
			job: &storage.ReviewJob{
				ID:      22,
				JobType: storage.JobTypeClassify,
				Status:  storage.JobStatusCanceled,
				Source:  "auto_design",
			},
			want:    []string{"canceled"},
			notWant: []string{"in progress"},
		},
		{
			// Clean classifier verdict (applyClassifyVerdict path):
			// MarkClassifyAsSkippedDesign with empty errorDetail.
			name: "skipped auto_design with reason",
			job: &storage.ReviewJob{
				ID:         3,
				JobType:    storage.JobTypeReview,
				Status:     storage.JobStatusSkipped,
				Source:     "auto_design",
				SkipReason: "trivial diff",
			},
			want:    []string{"no design review needed", "trivial diff"},
			notWant: []string{"failed"},
		},
		{
			// Daemon path: classifier execution fails →
			// completeClassifyAsSkip → MarkClassifyAsSkippedDesign
			// produces a row with status=skipped, source=auto_design,
			// AND a non-empty Error. Must NOT be labeled as a clean
			// "no design review needed" verdict.
			name: "skipped auto_design with classifier error",
			job: &storage.ReviewJob{
				ID:         4,
				JobType:    storage.JobTypeReview,
				Status:     storage.JobStatusSkipped,
				Source:     "auto_design",
				SkipReason: "classifier unavailable",
				Error:      "classify_agent \"codex\" not registered",
			},
			want: []string{
				"classifier failed",
				"classifier unavailable",
				"not registered",
			},
			notWant: []string{
				"no design review needed",
			},
		},
		{
			name: "skipped non-auto_design row leaves log alone",
			job: &storage.ReviewJob{
				ID:         5,
				JobType:    storage.JobTypeReview,
				Status:     storage.JobStatusSkipped,
				Source:     "",
				SkipReason: "unrelated reason",
			},
			wantEmpty: true,
		},
		{
			// Future pipeline could use classify job_type for unrelated
			// routing; mislabeling those as auto-design output would be
			// wrong.
			name: "non-auto_design classify row leaves log alone",
			job: &storage.ReviewJob{
				ID:      6,
				JobType: storage.JobTypeClassify,
				Status:  storage.JobStatusRunning,
				Source:  "",
			},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Width 0 = no truncation; tests use raw content
			// substrings.
			got := classifyReasoningLines(tt.job, 0)
			if tt.wantEmpty {
				assert.Empty(t, got,
					"expected no header lines for non-classify job")
				return
			}
			joined := strings.Join(got, "\n")
			for _, want := range tt.want {
				assert.Contains(t, joined, want,
					"reasoning header should mention %q", want)
			}
			for _, notWant := range tt.notWant {
				assert.NotContains(t, joined, notWant,
					"reasoning header should NOT mention %q", notWant)
			}
		})
	}
}

// TestClassifyReasoningLinesFoldsNewlines locks in that embedded
// newlines/tabs in raw classifier error text are folded to spaces so
// each returned line stays on a single terminal row. job.Error is
// stored raw (sanitization only applies to skip_reason at storage
// entry), and a multiline blob would break the row-reservation
// invariant in logVisibleLines.
func TestClassifyReasoningLinesFoldsNewlines(t *testing.T) {
	job := &storage.ReviewJob{
		ID:         7,
		JobType:    storage.JobTypeReview,
		Status:     storage.JobStatusSkipped,
		Source:     "auto_design",
		SkipReason: "line one\nline two",
		Error:      "exec: classifier\n\tstderr=timeout\n\tcode=124",
	}
	got := classifyReasoningLines(job, 0)
	assert.Len(t, got, 3)
	for i, line := range got {
		assert.NotContains(t, line, "\n",
			"line %d contains a newline: %q", i, line)
		assert.NotContains(t, line, "\r",
			"line %d contains a carriage return: %q", i, line)
		assert.NotContains(t, line, "\t",
			"line %d contains a tab: %q", i, line)
	}
}

// TestClassifyReasoningLinesTruncation locks in the contract that
// renderLogView and logVisibleLines both rely on: each returned line
// occupies exactly one terminal row. If the lines could wrap, the
// reserved-space calculation would be wrong and the log content area
// would misalign.
func TestClassifyReasoningLinesTruncation(t *testing.T) {
	longReason := strings.Repeat("very long classifier reason ", 8)
	longError := strings.Repeat("internal error blob ", 12)
	job := &storage.ReviewJob{
		ID:         6,
		JobType:    storage.JobTypeReview,
		Status:     storage.JobStatusSkipped,
		Source:     "auto_design",
		SkipReason: longReason,
		Error:      longError,
	}
	const width = 80

	got := classifyReasoningLines(job, width)
	assert.Len(t, got, 3, "expected verdict + reason + detail rows")
	for i, line := range got {
		// Strip ANSI styling to measure printable width.
		stripped := ansi.PrintableRuneWidth(line)
		// runewidth.StringWidth on the rendered line includes ANSI
		// codes so use ansi.PrintableRuneWidth for the assertion;
		// fall back to a runewidth check on a stripped copy if
		// PrintableRuneWidth returns 0 (older versions).
		if stripped == 0 {
			stripped = runewidth.StringWidth(line)
		}
		assert.LessOrEqual(t, stripped, width,
			"line %d (%q) exceeds width %d", i, line, width)
	}
}
