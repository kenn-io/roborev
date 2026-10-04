package goalreview

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestParseResultPreservesFindingsWithInvalidLocations(t *testing.T) {
	snapshot := Snapshot{
		Source: "superpowers", Stage: "spec",
		Artifacts: []Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\nRequirement"}},
		Issues:    []kata.Issue{{ShortID: "aaaa"}},
	}
	for _, tc := range []struct {
		name     string
		location string
		want     Location
	}{
		{"valid artifact", `{"file":"spec.md","line":2}`, Location{File: "spec.md", Line: 2}},
		{"missing line", `{"file":"spec.md"}`, Location{File: "spec.md"}},
		{"negative line", `{"file":"spec.md","line":-1}`, Location{File: "spec.md"}},
		{"line past artifact", `{"file":"spec.md","line":3}`, Location{File: "spec.md"}},
		{"unknown file", `{"file":"other.md","line":1}`, Location{}},
		{"valid Kata id", `{"kata_id":"aaaa"}`, Location{KataID: "aaaa"}},
		{"unknown Kata id", `{"kata_id":"unknown"}`, Location{}},
		{"ambiguous anchors", `{"file":"spec.md","line":1,"kata_id":"aaaa"}`, Location{}},
		{"missing anchor", `{}`, Location{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"findings":[{"severity":"high","message":"Contradiction","fix":"Resolve the conflicting requirements.","location":` + tc.location + `}]}`
			findings, err := ParseResult(raw, snapshot)
			require.NoError(t, err)
			assert.Equal(t, []Finding{{Severity: "high", Message: "Contradiction", Fix: "Resolve the conflicting requirements.", Location: tc.want}}, findings)
		})
	}
}

func TestParseResultRetainsMechanicalFindingsAfterInvalidLocation(t *testing.T) {
	snapshot := exampleSnapshot()
	snapshot.Artifacts[1].Content = "### Task 1: Work\n"
	findings, err := ParseResult(`{"findings":[{"severity":"high","message":"Contradiction","fix":"Resolve the conflicting requirements.","location":{"file":"unknown.md","line":1}}]}`, snapshot)
	require.NoError(t, err)
	require.Len(t, findings, 2)
	assert.Equal(t, Finding{Severity: "high", Message: "Contradiction", Fix: "Resolve the conflicting requirements."}, findings[0])
	assert.Equal(t, planPath, findings[1].Location.File)
}

func TestRenderPartiallyLocatedFindings(t *testing.T) {
	assert.Equal(t, "- high: Contradiction Fix: Resolve the conflicting requirements.", Render([]Finding{{Severity: "high", Message: "Contradiction", Fix: "Resolve the conflicting requirements."}}))
	assert.Equal(t, "- medium: spec.md: Missing requirement Fix: Add the missing requirement.", Render([]Finding{{Severity: "medium", Message: "Missing requirement", Fix: "Add the missing requirement.", Location: Location{File: "spec.md"}}}))
}

func TestDocumentPreservesGoalFindings(t *testing.T) {
	findings := []Finding{
		{Severity: "high", Message: "Contradiction", Fix: "Resolve it.", Location: Location{File: "spec.md", Line: 2}},
		{Severity: "medium", Message: "Missing requirement", Fix: "Add it.", Location: Location{File: "spec.md"}},
		{Severity: "low", Message: "Conflicting task", Fix: "Align it.", Location: Location{KataID: "aaaa"}},
		{Severity: "low", Message: "No issues found. is an unsupported conclusion", Fix: "Provide evidence."},
	}
	doc := Document(findings)
	assert.Equal(t, structuredreview.VerdictFail, doc.Verdict)
	assert.Equal(t, []structuredreview.Finding{
		{Severity: "high", Problem: "Contradiction", Fix: "Resolve it.", Location: "spec.md:2"},
		{Severity: "medium", Problem: "Missing requirement", Fix: "Add it.", Location: "spec.md"},
		{Severity: "low", Problem: "Conflicting task", Fix: "Align it.", Location: "Kata aaaa"},
		{Severity: "low", Problem: "No issues found. is an unsupported conclusion", Fix: "Provide evidence."},
	}, doc.Findings)
	assert.Equal(t, structuredreview.VerdictPass, Document(nil).Verdict)
}

func TestFindingsFromDocumentRestoresCapturedLocations(t *testing.T) {
	snapshot := Snapshot{
		Artifacts: []Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\nRequirement"}},
		Issues:    []kata.Issue{{ShortID: "aaaa"}},
	}
	for _, tc := range []struct {
		location string
		want     Location
	}{
		{"spec.md:2", Location{File: "spec.md", Line: 2}},
		{"spec.md", Location{File: "spec.md"}},
		{"spec.md:3", Location{File: "spec.md"}},
		{"Kata aaaa", Location{KataID: "aaaa"}},
		{"Kata unknown", Location{}},
		{"unknown.md:1", Location{}},
		{"", Location{}},
	} {
		t.Run(tc.location, func(t *testing.T) {
			doc := structuredreview.Document{Findings: []structuredreview.Finding{{
				Severity: "medium", Problem: "Contradiction", Fix: "Resolve it.", Location: tc.location,
			}}}
			assert.Equal(t, []Finding{{Severity: "medium", Message: "Contradiction", Fix: "Resolve it.", Location: tc.want}}, FindingsFromDocument(doc, snapshot))
		})
	}
}

func TestFindingsFromDocumentKeepsExactArtifactPaths(t *testing.T) {
	for _, name := range []string{"Kata notes.md", "notes:1"} {
		snapshot := Snapshot{Artifacts: []Artifact{{Kind: "spec", Path: name, Content: "# Feature"}}}
		doc := structuredreview.Document{Findings: []structuredreview.Finding{{
			Severity: "high", Problem: "Contradiction", Fix: "Resolve it.", Location: name,
		}}}
		assert.Equal(t, []Finding{{Severity: "high", Message: "Contradiction", Fix: "Resolve it.", Location: Location{File: name}}}, FindingsFromDocument(doc, snapshot))
	}
}

func TestGoalFindingDocumentRoundTripPreservesKataPrefixedFile(t *testing.T) {
	snapshot := Snapshot{Artifacts: []Artifact{{Kind: "spec", Path: "Kata notes.md", Content: "# Feature"}}}
	findings := []Finding{{Severity: "high", Message: "Contradiction", Fix: "Resolve it.", Location: Location{File: "Kata notes.md", Line: 1}}}
	assert.Equal(t, findings, FindingsFromDocument(Document(findings), snapshot))
}
