package prompt

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// A renderer or fix-context builder that shortens either field would lose part
// of the repair contract. These checks exercise transport, not model quality.
func TestBehaviorHandoffSurvivesAddressAndPlanning(t *testing.T) {
	for _, example := range []struct {
		name, kind, topic, problem, fix string
	}{
		{
			"cross-tenant", "defect", "security",
			"When tenant A requests tenant B's invoice, the handler returns B's invoice because lookup ignores the caller's tenant.",
			"Reject cross-tenant access while preserving access to the caller's own invoices.\nProposed check, not run: request an own invoice and another tenant's invoice.",
		},
		{
			"document", "improvement", "documentation",
			"The retention guide promises both immediate deletion and a thirty-day recovery window; readers cannot tell which policy applies.",
			"Describe the actual retention policy consistently while preserving the explanation of account recovery. No runtime reproduction applies.",
		},
		{
			"test-improvement", "improvement", "tests",
			"The boundary case has no coverage; the current implementation may already handle it correctly.",
			"Add coverage distinguishing the boundary from its neighbors without claiming a demonstrated runtime failure. Proposed test, not run.",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			repo := t.TempDir()
			// A local config avoids linked-worktree discovery (a git subprocess)
			// on every generated case. The context and repository are fixed.
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), nil, 0o600))
			builder := NewBuilder(nil).WithContext(t.Context()).ForRepo(repo, 0)
			property := func(context, caveat string, severityIndex, thresholdIndex uint8) bool {
				assert := assert.New(t)
				problem := strings.TrimSpace(example.problem + "\nContext: " + context)
				fix := strings.TrimSpace(example.fix + "\nCaveat: " + caveat)
				severity := []string{"critical", "high", "medium", "low"}[severityIndex%4]
				threshold := []string{"", "low", "medium", "high", "critical"}[thresholdIndex%5]
				document := structuredreview.Document{
					Summary: "Synthetic review", Verdict: structuredreview.VerdictFail,
					Findings: []structuredreview.Finding{{
						Severity: severity, Kind: example.kind, Topic: example.topic,
						Confidence: 0.7, Problem: problem, Fix: fix,
					}},
				}
				raw, err := json.Marshal(document)
				require.NoError(t, err)
				decoded, err := structuredreview.Decode(raw)
				require.NoError(t, err)
				require.Len(t, decoded.Findings, 1)
				assert.Equal(problem, decoded.Findings[0].Problem)
				assert.Equal(fix, decoded.Findings[0].Fix)
				// Severity changes the verdict/instruction, never the finding text.
				markdown := decoded.Markdown(threshold)
				review := &storage.Review{JobID: 7, Output: markdown}
				address, err := builder.BuildAddressPrompt(review, nil, threshold)
				require.NoError(t, err)
				plan, err := builder.BuildPlanPrompt(review, nil, threshold)
				require.NoError(t, err)
				for _, body := range []string{markdown, address, plan} {
					assert.Contains(body, "**Problem:** "+problem+"\n\n**Fix:** "+fix+"\n")
				}
				return !t.Failed()
			}
			require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
		})
	}
}
