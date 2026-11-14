package rebuttal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestSelectionBinding(t *testing.T) {
	s := Selection{ReviewUUID: "review-version", Finding: "bug\n\ttext"}
	prompt, err := EncodeSelection(s, "quoted input")
	require.NoError(t, err)
	got, err := DecodeSelection(prompt)
	require.NoError(t, err)
	assert.Equal(t, s, got)
	_, err = DecodeSelection("Original finding: " + prompt)
	require.Error(t, err)
	_, err = DecodeSelection("old unbound prompt")
	require.Error(t, err)
}

func TestMatchFindingUnique(t *testing.T) {
	fs := []structuredreview.Finding{{Severity: "high", Problem: "bug text", Fix: "repair"}, {Severity: "low", Problem: "other bug", Fix: "repair"}}
	assert.Equal(t, 0, MatchFinding("bug\n text", fs))
	assert.Equal(t, -1, MatchFinding("repair", fs))
	assert.Equal(t, -1, MatchFinding("", fs))
	assert.Equal(t, -1, MatchFinding("missing", fs))
}

func TestMatchFindingRenderedReview(t *testing.T) {
	doc := structuredreview.Document{
		Summary: "Review results",
		Findings: []structuredreview.Finding{
			{Severity: "high", Location: "worker.go:12", Problem: "Cancellation loses the result", Fix: "Save before returning"},
			{Severity: "medium", Problem: "The retry repeats a completed operation", Fix: "Check completion before retrying"},
			{Severity: "low", Location: "output.go:8", Problem: "The count includes unrelated records", Fix: "Filter by repository"},
		},
	}
	_, findings, ok := strings.Cut(doc.Markdown(""), "## Findings\n")
	require.True(t, ok)
	excerpts := strings.Split(strings.TrimSpace(findings), "\n### ")
	require.Len(t, excerpts, len(doc.Findings))
	for i, excerpt := range excerpts {
		if i > 0 {
			excerpt = "### " + excerpt
		}
		assert.Equal(t, i, MatchFinding(excerpt, doc.Findings))
	}
	assert.Equal(t, -1, MatchFinding(findings, doc.Findings))
}

func FuzzSelection(f *testing.F) {
	f.Add("version", "finding", "body")
	f.Add("v", "\n marker \x00", "quoted\ntext")
	f.Fuzz(func(t *testing.T, version, finding, body string) {
		s := Selection{ReviewUUID: version, Finding: finding}
		prompt, err := EncodeSelection(s, body)
		if err != nil {
			return
		}
		got, err := DecodeSelection(prompt)
		require.NoError(t, err)
		assert.Equal(t, s, got)
		_, err = DecodeSelection("quoted: " + prompt)
		require.Error(t, err)
	})
}

func FuzzMatchFinding(f *testing.F) {
	f.Add("a bug", "repair")
	f.Fuzz(func(t *testing.T, problem, fix string) {
		fs := []structuredreview.Finding{{Severity: "high", Problem: problem, Fix: fix}}
		idx := MatchFinding(problem, fs)
		if strings.TrimSpace(problem) == "" {
			assert.Equal(t, -1, idx)
		} else {
			assert.Equal(t, 0, idx)
		}
		assert.Equal(t, -1, MatchFinding(problem, append(fs, fs...)))
	})
}

func TestSelectionRejectsMalformedBindings(t *testing.T) {
	for _, prompt := range []string{"roborev-rebuttal-selection-v1:!\nbody", "roborev-rebuttal-selection-v1:e30\nbody", "roborev-rebuttal-selection-v1:eyJvdGhlciI6MX0\nbody"} {
		_, err := DecodeSelection(prompt)
		require.Error(t, err)
	}
	_, err := EncodeSelection(Selection{ReviewUUID: "version", Finding: " \n"}, "body")
	require.Error(t, err)
}
