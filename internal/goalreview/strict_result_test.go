package goalreview

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResultCaseAliasCannotEraseFindings(t *testing.T) {
	raw := `{"findings":[{"severity":"high","message":"Contradiction","location":{"file":"spec.md","line":1}}],"Findings":[]}`
	snapshot := Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	findings, err := ParseResult(raw, snapshot)
	require.Error(t, err, "case-mismatched duplicate must not erase a high finding; got %s", Render(findings))
}
