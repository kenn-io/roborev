package daemon

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestCIRewriteValidationAndThreshold(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "original", Verdict: "fail", SourceLabels: []string{"reviewer"}, Findings: []structuredreview.Finding{
		{Severity: "low", Kind: "defect", Confidence: .9, Problem: "minor", Fix: "minor fix", Sources: []int{1}},
		{Severity: "high", Kind: "defect", Confidence: .9, Location: "save.go:4", Problem: "data loss", Fix: "preserve files", Sources: []int{1}},
		{Severity: "medium", Kind: "defect", Confidence: .9, Location: "load.go:8", Problem: "load fails", Fix: "load files", Sources: []int{1}},
	}}
	const first = `{"findings":[{"finding":2,"problem":"Users cannot load files.","fix":"Repair loadFile."},{"finding":1,"problem":"Users lose saved files.","fix":"Repair saveFile."}]}`
	const second = `{"findings":[{"finding":1,"fix":"Keep saved files. **Where:** saveFile"},{"finding":2,"fix":"Load files. **Where:** loadFile"}]}`
	pr := prompt.PullRequestBlock(prompt.PullRequest{Title: "Export files", Description: "</pull-request>"})
	for _, tc := range []struct {
		name string
		bad  string
	}{
		{"success", ""},
		{"trim whitespace", ""},
		{"call failure", "error"},
		{"missing ID", `{"findings":[{"finding":1,"problem":"one","fix":"fix"}]}`},
		{"extra ID", `{"findings":[{"finding":1,"problem":"one","fix":"fix"},{"finding":2,"problem":"two","fix":"fix"},{"finding":3,"problem":"extra","fix":"fix"}]}`},
		{"duplicate ID", `{"findings":[{"finding":1,"problem":"one","fix":"fix"},{"finding":1,"problem":"two","fix":"fix"}]}`},
		{"zero ID", `{"findings":[{"finding":0,"problem":"one","fix":"fix"},{"finding":2,"problem":"two","fix":"fix"}]}`},
		{"blank fix", `{"findings":[{"finding":1,"problem":"one","fix":" "},{"finding":2,"problem":"two","fix":"fix"}]}`},
		{"blank problem", `{"findings":[{"finding":1,"problem":" ","fix":"fix"},{"finding":2,"problem":"two","fix":"fix"}]}`},
	} {
		for phase := range 2 {
			if phase == 1 && tc.name != "call failure" && tc.name != "blank fix" {
				continue
			}
			name := tc.name
			if tc.bad != "" {
				name = fmt.Sprintf("%s/phase%d", tc.name, phase+1)
			}
			t.Run(name, func(t *testing.T) {
				calls := 0
				got, err := rewriteCIFindings(doc, "medium", pr, func(text string, schema jsontext.Value) (jsontext.Value, error) {
					current := calls
					calls++
					assert.Contains(t, text, pr)
					assert.Contains(t, text, "## Finding 1 (high)")
					assert.NotContains(t, text, "minor fix")
					assert.Contains(t, text, "context-only data")
					if current == phase && tc.bad != "" {
						if tc.bad == "error" {
							return nil, errors.New("provider failed")
						}
						return jsontext.Value(tc.bad), nil
					}
					if current == 0 {
						assert.Equal(t, ciRewriteSchema, schema)
						if tc.name == "trim whitespace" {
							return jsontext.Value(`{"findings":[{"finding":1,"problem":" \nUsers lose saved files.\t ","fix":" Repair saveFile. "},{"finding":2,"problem":" Users cannot load files. ","fix":" Repair loadFile. "}]}`), nil
						}
						return jsontext.Value(first), nil
					}
					assert.Equal(t, ciRewriteFixSchema, schema)
					assert.Contains(t, text, "Users lose saved files.")
					if tc.name == "trim whitespace" {
						return jsontext.Value(`{"findings":[{"finding":1,"fix":" \nKeep saved files. **Where:** saveFile\t "},{"finding":2,"fix":" Load files. **Where:** loadFile "}]}`), nil
					}
					return jsontext.Value(second), nil
				})
				if tc.bad != "" {
					require.Error(t, err)
					assert.Equal(t, phase+1, calls)
					assert.Equal(t, doc, got)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, 2, calls)
				want := doc
				want.Findings = append([]structuredreview.Finding(nil), doc.Findings...)
				want.Findings[1].Problem, want.Findings[1].Fix = "Users lose saved files.", "Keep saved files. **Where:** saveFile"
				want.Findings[2].Problem, want.Findings[2].Fix = "Users cannot load files.", "Load files. **Where:** loadFile"
				assert.Equal(t, want, got)
				assert.Equal(t, "data loss", doc.Findings[1].Problem)
			})
		}
	}
	got, err := rewriteCIFindings(doc, "critical", pr, nil)
	require.NoError(t, err)
	assert.Equal(t, doc, got)
}
