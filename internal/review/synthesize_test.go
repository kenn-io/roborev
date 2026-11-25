package review

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	assert.Contains(t, s, substr)
}

// commonMockAgent provides default no-op chaining methods
type commonMockAgent struct {
	self agent.Agent
}

func (a *commonMockAgent) WithReasoning(_ agent.ReasoningLevel) agent.Agent { return a.self }
func (a *commonMockAgent) WithAgentic(_ bool) agent.Agent                   { return a.self }
func (a *commonMockAgent) WithModel(_ string) agent.Agent                   { return a.self }

// failingSynthAgent always returns an error from Review,
// used to force synthesis fallback deterministically.
type failingSynthAgent struct {
	commonMockAgent
}

func newFailingSynthAgent() *failingSynthAgent {
	a := &failingSynthAgent{}
	a.self = a
	return a
}

func (a *failingSynthAgent) Name() string { return "failing-synth" }
func (a *failingSynthAgent) Review(
	_ context.Context, _, _, _ string, _ io.Writer,
) (string, error) {
	return "", errors.New("agent exploded")
}
func (a *failingSynthAgent) CommandLine() string { return "failing-synth" }

// capturingAgent records the gitRef passed to Review.
type capturingAgent struct {
	commonMockAgent
	capturedGitRef string
}

func newCapturingAgent() *capturingAgent {
	a := &capturingAgent{}
	a.self = a
	return a
}

func (a *capturingAgent) Name() string { return "capture" }
func (a *capturingAgent) Review(
	_ context.Context, _, gitRef, _ string, _ io.Writer,
) (string, error) {
	a.capturedGitRef = gitRef
	return `{"schema_version":4,"summary":"synthesized output","verdict":"pass","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":false,"problem":"combined","fix":"fix","file":"file.go","start_line":1,"end_line":1,"sources":[1],"source_findings":[]}]}`, nil
}
func (a *capturingAgent) CommandLine() string { return "capture" }

func TestSynthesisPreservesMatchingSourceUnlikely(t *testing.T) {
	rare := StructuredFinding{Severity: "medium", Kind: "defect", Confidence: 0.8, Problem: "Rare issue", Fix: "Fix rare", Unlikely: true}
	rare.SetLocation("file.go", 1, 1)
	ordinary := StructuredFinding{Severity: "medium", Kind: "defect", Confidence: 0.8, Problem: "Ordinary issue", Fix: "Fix ordinary"}
	ordinary.SetLocation("file.go", 20, 20)
	for _, tc := range []struct {
		name, output string
		reviews      []ReviewResult
		want         []StructuredFinding
	}{
		{"unlikely", `{"schema_version":4,"summary":"Review","verdict":"fail","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":false,"problem":"Rare issue","fix":"Fix rare","file":"file.go","start_line":1,"end_line":1,"sources":[1],"source_findings":[{"review":1,"finding":1}]},{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":false,"problem":"Ordinary issue","fix":"Fix ordinary","file":"file.go","start_line":20,"end_line":20,"sources":[1],"source_findings":[{"review":1,"finding":2}]}]}`, []ReviewResult{{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{rare, ordinary}}}}, []StructuredFinding{{Problem: "Rare issue", Unlikely: true}, {Problem: "Ordinary issue"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &structuredBatchAgent{name: "visibility-synthesis", result: []byte(tc.output)}
			doc, _, err := RunSynthesisAgent(t.Context(), a, tc.reviews, "prompt", "", nil, SynthesisHooks{})
			require.NoError(t, err)
			require.Len(t, doc.Findings, len(tc.want))
			for i, want := range tc.want {
				assert.Equal(t, want.Problem, doc.Findings[i].Problem)
				assert.Equal(t, want.Topic, doc.Findings[i].Topic)
				assert.Equal(t, want.Unlikely, doc.Findings[i].Unlikely)
			}
		})
	}
}

func TestSynthesisPreservesRewordedUnlocatedSecurity(t *testing.T) {
	source := StructuredFinding{Severity: "medium", Topic: "security", Problem: "Original security issue", Fix: "Fix original"}
	ordinary := StructuredFinding{Severity: "medium", Topic: "performance", Problem: "Different issue", Fix: "Fix it"}
	reviews := []ReviewResult{{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{source, ordinary}}}}
	output := `{"schema_version":4,"summary":"Review","verdict":"fail","findings":[{"severity":"medium","kind":"defect","topic":"performance","confidence":0.8,"unlikely":true,"problem":"Reworded issue","fix":"Fix it","file":null,"start_line":null,"end_line":null,"sources":[1],"source_findings":[{"review":1,"finding":1}]},{"severity":"medium","kind":"defect","topic":"performance","confidence":0.8,"unlikely":false,"problem":"Other issue","fix":"Fix it","file":null,"start_line":null,"end_line":null,"sources":[1],"source_findings":[{"review":1,"finding":2}]}]}`
	a := &structuredBatchAgent{name: "visibility-synthesis", result: []byte(output)}
	doc, _, err := RunSynthesisAgent(t.Context(), a, reviews, "prompt", "", nil, SynthesisHooks{})
	require.NoError(t, err)
	require.Len(t, doc.Findings, 2)
	assert.Equal(t, "security", doc.Findings[0].Topic)
	assert.Equal(t, "performance", doc.Findings[1].Topic)
	assert.False(t, doc.Findings[1].Unlikely)
	assert.Contains(t, FormatComment(PrepareComment(CommentConfig{}, ReviewResult{Structured: &doc})), "Changes require fixes")
	stored, err := json.Marshal(doc)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "source_findings")
}

func TestSynthesisRejectsInvalidFindingReferences(t *testing.T) {
	reviews := []ReviewResult{{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{{Problem: "One"}}}}}
	for _, refs := range []string{`[{"review":1,"finding":2}]`, `[{"review":2,"finding":1}]`} {
		t.Run(refs, func(t *testing.T) {
			output := `{"schema_version":4,"summary":"Review","verdict":"fail","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":false,"problem":"Issue","fix":"Fix","file":null,"start_line":null,"end_line":null,"sources":[1],"source_findings":` + refs + `}]}`
			a := &structuredBatchAgent{name: "invalid-reference", result: []byte(output)}
			_, _, err := RunSynthesisAgent(t.Context(), a, reviews, "prompt", "", nil, SynthesisHooks{})
			require.Error(t, err)
		})
	}
}

func TestSynthesisAcceptsMissingOrIncompleteFindingReferences(t *testing.T) {
	reviews := []ReviewResult{
		{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{{Problem: "One", Unlikely: true}}}},
		{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{{Problem: "Two"}}}},
	}
	for _, refs := range []string{"", `,"source_findings":[]`, `,"source_findings":[{"review":1,"finding":1}]`} {
		for _, mark := range []string{"false", "true"} {
			t.Run(refs+"/"+mark, func(t *testing.T) {
				output := `{"schema_version":4,"summary":"Review","verdict":"fail","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":` + mark + `,"problem":"Issue","fix":"Fix","file":null,"start_line":null,"end_line":null,"sources":[1,2]` + refs + `}]}`
				a := &structuredBatchAgent{name: "partial-reference", result: []byte(output)}
				doc, _, err := RunSynthesisAgent(t.Context(), a, reviews, "prompt", "", nil, SynthesisHooks{})
				require.NoError(t, err)
				require.Len(t, doc.Findings, 1)
				assert.Equal(t, mark == "true", doc.Findings[0].Unlikely)
			})
		}
	}
}

func TestSynthesisUnlikelyRequiresAllSources(t *testing.T) {
	reviews := []ReviewResult{
		{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{{Problem: "One", Unlikely: true}}}},
		{Status: ResultDone, Structured: &StructuredReview{Findings: []StructuredFinding{{Problem: "Two"}}}},
	}
	output := `{"schema_version":4,"summary":"Review","verdict":"fail","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":true,"problem":"Issue","fix":"Fix","file":null,"start_line":null,"end_line":null,"sources":[1,2],"source_findings":[{"review":1,"finding":1},{"review":2,"finding":1}]}]}`
	a := &structuredBatchAgent{name: "mixed-sources", result: []byte(output)}
	doc, _, err := RunSynthesisAgent(t.Context(), a, reviews, "prompt", "", nil, SynthesisHooks{})
	require.NoError(t, err)
	require.Len(t, doc.Findings, 1)
	assert.False(t, doc.Findings[0].Unlikely)
}

func TestSynthesisSchemaAllowsFindingReferences(t *testing.T) {
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(SynthesisSchema, &schema))
	validator, err := schema.Resolve(nil)
	require.NoError(t, err)
	document := `{"schema_version":4,"summary":"Review","verdict":"fail","findings":[{"severity":"medium","kind":"defect","topic":null,"confidence":0.8,"unlikely":false,"problem":"Issue","fix":"Fix","file":null,"start_line":null,"end_line":null,"sources":[1],"source_findings":[{"review":1,"finding":1}]}]}`
	var value any
	require.NoError(t, json.Unmarshal([]byte(document), &value))
	assert.NoError(t, validator.Validate(value))
	missing := strings.Replace(document, `,"source_findings":[{"review":1,"finding":1}]`, "", 1)
	require.NoError(t, json.Unmarshal([]byte(missing), &value))
	assert.NoError(t, validator.Validate(value))
}

type synthesisEntrypointAgent struct {
	commonMockAgent
	synthPrompt  string
	reviewCalled bool
}

func newSynthesisEntrypointAgent() *synthesisEntrypointAgent {
	a := &synthesisEntrypointAgent{}
	a.self = a
	return a
}

func (a *synthesisEntrypointAgent) Name() string { return "synthesis-entrypoint" }
func (a *synthesisEntrypointAgent) Review(
	_ context.Context, _, _, prompt string, _ io.Writer,
) (string, error) {
	a.reviewCalled = true
	a.synthPrompt = prompt
	return `{"schema_version":4,"summary":"synthesized output","verdict":"pass","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":false,"problem":"combined","fix":"fix","file":"file.go","start_line":1,"end_line":1,"sources":[1],"source_findings":[]}]}`, nil
}
func (a *synthesisEntrypointAgent) CommandLine() string { return "synthesis-entrypoint" }

type structuredSynthesisAgent struct {
	commonMockAgent
	schema       jsontext.Value
	repoPath     string
	reviewCalled bool
}

func newStructuredSynthesisAgent() *structuredSynthesisAgent {
	a := &structuredSynthesisAgent{}
	a.self = a
	return a
}

func (a *structuredSynthesisAgent) Name() string { return "structured-synthesis" }
func (a *structuredSynthesisAgent) Review(
	context.Context, string, string, string, io.Writer,
) (string, error) {
	a.reviewCalled = true
	return "", errors.New("plain review must not be used when ReviewWithSchema exists")
}

func (a *structuredSynthesisAgent) ClassifyWithSchema(context.Context, string, string, string, jsontext.Value, io.Writer) (jsontext.Value, error) {
	return nil, errors.New("synthesis uses the ordinary review entrypoint")
}

func (a *structuredSynthesisAgent) ReviewWithSchema(
	_ context.Context, repoPath, _, _ string, schema jsontext.Value, _ io.Writer,
) (jsontext.Value, error) {
	a.schema = schema
	a.repoPath = repoPath
	return jsontext.Value(`{"schema_version":4,"summary":"synthesized output","verdict":"pass","findings":[{"severity":"medium","kind":"defect","confidence":0.8,"unlikely":false,"problem":"combined","fix":"fix","file":"file.go","start_line":1,"end_line":1,"sources":[1],"source_findings":[]}]}`), nil
}
func (a *structuredSynthesisAgent) CommandLine() string { return a.Name() }

func TestRunSynthesisAgentPrefersSchemaReviewOverPlainReview(t *testing.T) {
	a := newStructuredSynthesisAgent()
	reviews := []ReviewResult{{Agent: "codex", Status: ResultDone, Output: "Found issue A"}}
	cleaned := false

	doc, _, err := RunSynthesisAgent(t.Context(), a, reviews, "prompt", "", nil, SynthesisHooks{
		Checkout: func() (SynthesisCheckout, error) {
			return SynthesisCheckout{RepoPath: "/repo", GitRef: "HEAD", Cleanup: func() { cleaned = true }}, nil
		},
	})
	require.NoError(t, err)
	assert := assert.New(t)
	assert.False(a.reviewCalled)
	assert.JSONEq(string(SynthesisSchema), string(a.schema))
	assert.Equal("/repo", a.repoPath, "schema review runs against the reviewed checkout")
	assert.True(cleaned, "checkout cleanup must run after the agent")
	assert.Equal([]int{1}, doc.Findings[0].Sources)
}

type invalidSynthesisAgent struct {
	commonMockAgent
}

func newInvalidSynthesisAgent() *invalidSynthesisAgent {
	a := &invalidSynthesisAgent{}
	a.self = a
	return a
}

func (a *invalidSynthesisAgent) Name() string { return "invalid-synthesis" }
func (a *invalidSynthesisAgent) Review(
	context.Context, string, string, string, io.Writer,
) (string, error) {
	return "Markdown without structured synthesis JSON.", nil
}
func (a *invalidSynthesisAgent) CommandLine() string { return a.Name() }

func TestSynthesize_Formatting(t *testing.T) {
	tests := []struct {
		name          string
		results       []ReviewResult
		expectedErr   error
		expectedTexts []string
	}{
		{
			name: "AllFailed",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     "failed",
					Error:      "agent crashed",
				},
			},
			expectedErr: ErrAllFailed,
			expectedTexts: []string{
				"Review Failed",
			},
		},
		{
			name: "SingleSuccess",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     "done",
					Output:     "No issues found.",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Passed",
				"No issues found.",
			},
		},
		{
			name: "SingleStructuredSuccess",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "custom",
					Status:     "done",
					Output:     "High: no actionable findings.",
					Verdict:    storage.VerdictPass,
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Passed",
				"High: no actionable findings.",
			},
		},
		{
			name: "SingleStructuredFailure",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "custom",
					Status:     "done",
					Output:     "No issues found.",
					Verdict:    storage.VerdictFail,
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Complete",
				"No issues found.",
			},
		},
		{
			name: "SingleSeverityThresholdMet",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "review",
					Status:     "done",
					Output:     "SEVERITY_THRESHOLD_MET",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Passed",
			},
		},
		{
			name: "ThresholdMetWithFindings",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "review",
					Status:     "done",
					Output:     "SEVERITY_THRESHOLD_MET\n\n- High: critical bug found",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Complete",
			},
		},
		{
			name: "AllQuota",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     "failed",
					Error:      QuotaErrorPrefix + "exhausted",
				},
			},
			expectedErr: nil,
			expectedTexts: []string{
				"Review Skipped",
			},
		},
		{
			name: "EmptyOnly",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     ResultDone,
					Output:     "No review output generated",
				},
			},
			expectedTexts: []string{
				"Review Skipped",
			},
		},
		{
			name: "PlaceholderAndFailure",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     ResultDone,
					Output:     "No review output generated",
				},
				{
					Agent:      "gemini",
					ReviewType: "security",
					Status:     ResultFailed,
					Error:      "agent crashed",
				},
			},
			expectedErr: ErrAllFailed,
			expectedTexts: []string{
				"Review Failed",
			},
		},
		{
			name: "WhitespaceAndFailure",
			results: []ReviewResult{
				{
					Agent:      "codex",
					ReviewType: "security",
					Status:     ResultDone,
					Output:     " \n\t",
				},
				{
					Agent:      "gemini",
					ReviewType: "security",
					Status:     ResultFailed,
					Error:      "agent crashed",
				},
			},
			expectedErr: ErrAllFailed,
			expectedTexts: []string{
				"Review Failed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synthesis, err := Synthesize(
				t.Context(), tt.results, SynthesizeOpts{
					HeadSHA: "abc123456789",
				})
			comment := synthesis.GitHubComment
			if tt.expectedErr != nil {
				require.ErrorIs(t, err, tt.expectedErr)
			} else {
				require.NoError(t, err)
			}
			for _, text := range tt.expectedTexts {
				assertContains(t, comment, text)
			}
			assert.NotContains(t, comment, "Agent:")
			assert.NotContains(t, comment, "Type:")
		})
	}
}

func TestSynthesize_MultipleResults_FallsBackToRaw(t *testing.T) {
	t.Parallel()
	// Register an agent that always fails so the test is
	// deterministic regardless of what other agents exist
	// in the global registry.
	ag := newFailingSynthAgent()
	agent.RegisterForTest(t, ag)

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "design",
			Status:     "done",
			Output:     "Design looks good",
		},
	}

	synthesis, err := Synthesize(
		t.Context(), results, SynthesizeOpts{
			Agent:   "failing-synth",
			HeadSHA: "def456789012",
		})
	comment := synthesis.GitHubComment
	require.NoError(t, err)

	// Should fall back to raw format
	assertContains(t, comment, "Synthesis unavailable")
	assertContains(t, comment, "Found issue A")
	assertContains(t, comment, "Design looks good")
	assert.NotContains(t, comment, "codex —")
	assert.NotContains(t, comment, "security")
	assert.NotContains(t, comment, "design")
}

func TestSynthesize_InvalidJSONFallsBackToRaw(t *testing.T) {
	t.Parallel()
	ag := newInvalidSynthesisAgent()
	agent.RegisterForTest(t, ag)

	results := []ReviewResult{
		{Status: ResultDone, Output: "Finding A"},
		{Status: ResultDone, Output: "Finding B"},
	}
	synthesis, err := Synthesize(t.Context(), results, SynthesizeOpts{
		Agent: ag.Name(), HeadSHA: "def456789012",
	})
	comment := synthesis.GitHubComment
	require.NoError(t, err)
	assert.Contains(t, comment, "Synthesis unavailable")
	assert.Contains(t, comment, "Finding A")
	assert.Contains(t, comment, "Finding B")
}

func TestSynthesize_MixedSuccessAndFailure(t *testing.T) {
	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found vulnerability",
		},
		{
			Agent:      "gemini",
			ReviewType: "security",
			Status:     "failed",
			Error:      "agent crashed",
		},
	}

	synthesis, err := Synthesize(
		t.Context(), results, SynthesizeOpts{
			Agent:   "nonexistent-synthesis-agent",
			HeadSHA: "abc123456789",
		})
	comment := synthesis.GitHubComment
	require.NoError(t, err)

	// Should fall back to raw format since synthesis agent
	// doesn't exist
	assertContains(t, comment, "Combined Review")
}

func TestSynthesize_PassesGitRefToAgent(t *testing.T) {
	t.Parallel()
	cap := newCapturingAgent()
	agent.RegisterForTest(t, cap)

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "design",
			Status:     "done",
			Output:     "Design looks good",
		},
	}

	_, err := Synthesize(
		t.Context(), results, SynthesizeOpts{
			Agent:   "capture",
			GitRef:  "aaa111..bbb222",
			HeadSHA: "bbb222",
		})
	require.NoError(t, err)
	assert.Equal(t, "aaa111..bbb222", cap.capturedGitRef, "gitRef")
}

func TestSynthesize_UsesReviewEntrypoint(t *testing.T) {
	t.Parallel()
	synth := newSynthesisEntrypointAgent()
	agent.RegisterForTest(t, synth)

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "default",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue B",
		},
	}

	synthesis, err := Synthesize(t.Context(), results, SynthesizeOpts{
		Agent:   "synthesis-entrypoint",
		GitRef:  "aaa111..bbb222",
		HeadSHA: "bbb222",
	})
	comment := synthesis.GitHubComment
	require.NoError(t, err)
	assertContains(t, comment, "`file.go:1`: combined\n\n  **Fix:** fix")
	assert.True(t, synth.reviewCalled, "synthesis uses the ordinary review entrypoint")
	assertContains(t, synth.synthPrompt, "Found issue A")
	assert.NotContains(t, synth.synthPrompt, "Review the code changes in commit")
}

func TestSynthesizeFiltersStructuredResultWithoutReparsingSummary(t *testing.T) {
	structured := StructuredReview{
		SchemaVersion: storage.StructuredReviewSchemaVersion,
		Summary:       "High: no actionable findings.",
		Findings: []StructuredFinding{{
			Severity: "low",
			Problem:  "Name is vague.",
			Fix:      "Rename it.",
		}},
	}
	result := ReviewResult{
		Agent:      "codex",
		ReviewType: "custom",
		Status:     ResultDone,
		Structured: &structured,
	}.ApplyMinSeverity("low")

	synthesis, err := Synthesize(t.Context(), []ReviewResult{result}, SynthesizeOpts{
		MinSeverity: "high",
		HeadSHA:     "abc123",
	})
	comment := synthesis.GitHubComment

	require.NoError(t, err)
	assert.Contains(t, comment, "Review Passed")
	assert.Contains(t, comment, "No findings at or above high severity.")
	assert.NotContains(t, comment, "Name is vague", "CI comments hide findings below the threshold")
}

func TestSynthesizePreservesCompleteOutput(t *testing.T) {
	doc := StructuredReview{SchemaVersion: 2, Summary: "Complete review.", Findings: []StructuredFinding{
		{Severity: "low", Problem: "Minor naming issue.", Fix: "Rename it."},
	}}
	output, err := Synthesize(t.Context(), []ReviewResult{{Status: ResultDone, Structured: &doc}}, SynthesizeOpts{MinSeverity: "high"})
	require.NoError(t, err)
	assert.Contains(t, output.Output, "Minor naming issue.")
}

func TestSynthesizeCommentsInheritReviewThreshold(t *testing.T) {
	doc := StructuredReview{SchemaVersion: 2, Summary: "Complete review.", Findings: []StructuredFinding{
		{Severity: "low", Problem: "Minor naming issue.", Fix: "Rename it."},
	}}
	for _, structured := range []bool{false, true} {
		for _, fallback := range []bool{false, true} {
			for _, ciSeverity := range []string{"", "low"} {
				result := ReviewResult{Status: ResultDone, Output: "### Low\nMinor naming issue."}
				if structured {
					result.Structured = &doc
				}
				result = result.ApplyMinSeverity("medium")
				results := []ReviewResult{result}
				if fallback {
					results = append(results, result)
				}
				synthesis, err := Synthesize(t.Context(), results, SynthesizeOpts{
					Agent: "nonexistent-synthesis-agent", MinSeverity: ciSeverity,
				})
				require.NoError(t, err)
				if ciSeverity == "" {
					assert.NotContains(t, synthesis.GitHubComment, "Minor naming issue.", "empty CI threshold inherits medium")
				} else {
					assert.Contains(t, synthesis.GitHubComment, "Minor naming issue.", "explicit low overrides medium")
				}
				assert.Contains(t, synthesis.Output, "Minor naming issue.")
			}
		}
	}
}

func TestSynthesizeGroupsVisibleFindings(t *testing.T) {
	t.Parallel()
	doc := StructuredReview{
		SchemaVersion: 2,
		Summary:       "Review summary includes a minor naming issue.",
		Verdict:       "fail",
		Findings: []StructuredFinding{
			{Severity: "low", Problem: "Minor naming issue.", Fix: "Rename it."},
			{Severity: "medium", Location: "worker.go:20", Problem: "Missing cleanup.", Fix: "Close the resource."},
			{Severity: "high", Location: "worker.go:10", Problem: "State is lost.", Fix: "Persist it."},
			{Severity: "medium", Problem: "Errors are ignored.", Fix: "Return the error."},
		},
	}
	for i := range doc.Findings {
		doc.Findings[i].Sources = []int{1}
		doc.Findings[i].Kind = "defect"
		doc.Findings[i].Confidence = 0.8
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	raw = []byte(strings.ReplaceAll(string(raw), `"sources":[1]}`, `"sources":[1],"unlikely":false}`))
	for _, mode := range []string{"single", "synthesis", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			synth := &structuredBatchAgent{result: raw}
			synth.name = "comment-synth"
			if mode == "fallback" {
				synth.err = errors.New("synthesis failed")
			}
			agent.RegisterForTest(t, synth)
			result := ReviewResult{Agent: "codex", Status: ResultDone, Structured: &doc}.ApplyMinSeverity("low")
			results := []ReviewResult{result}
			if mode != "single" {
				results = append(results, result)
			}
			synthesis, err := Synthesize(t.Context(), results, SynthesizeOpts{Agent: synth.Name(), MinSeverity: "medium"})
			comment := synthesis.GitHubComment
			require.NoError(t, err)
			assert := assert.New(t)
			assert.Contains(comment, "### High\n\n- `worker.go:10`: State is lost.\n\n  **Fix:** Persist it.")
			assert.Contains(comment, "### Medium\n\n- `worker.go:20`: Missing cleanup.\n\n  **Fix:** Close the resource.")
			assert.Contains(comment, "- Errors are ignored.\n\n  **Fix:** Return the error.")
			assert.NotContains(comment, "Minor naming issue.")
			assert.Contains(synthesis.Output, "Minor naming issue.", "complete output retains low findings in every path")
			assert.NotContains(comment, "Agent assessment")
			assert.Len(doc.Findings, 4, "presentation must preserve the stored findings")
			assert.Contains(doc.Markdown("medium"), "Minor naming issue.")
		})
	}
}

func TestSynthesize_EmptyAgentAutoSelectsAvailableAgent(t *testing.T) {
	t.Setenv("PATH", "")

	synth := newSynthesisEntrypointAgent()
	agent.RegisterForTest(t, synth)

	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "default",
			Status:     "done",
			Output:     "Found issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "Found issue B",
		},
	}

	synthesis, err := Synthesize(t.Context(), results, SynthesizeOpts{
		GitRef:  "aaa111..bbb222",
		HeadSHA: "bbb222",
	})
	comment := synthesis.GitHubComment
	require.NoError(t, err)
	assertContains(t, comment, "`file.go:1`: combined\n\n  **Fix:** fix")
	assert.True(t, synth.reviewCalled, "synthesis uses the ordinary review entrypoint")
}

func TestSynthesize_PassesGlobalConfigToResolver(t *testing.T) {
	originalResolver := getAvailableWithConfig
	t.Cleanup(func() { getAvailableWithConfig = originalResolver })

	var seenAgent string
	var seenCfg *config.Config
	cap := newCapturingAgent()
	getAvailableWithConfig = func(repoPath string, agentName string, cfg *config.Config, backups ...string) (agent.Agent, error) {
		seenAgent = agentName
		seenCfg = cfg
		return cap, nil
	}

	cfg := &config.Config{ACP: config.ACPAgentConfigs{
		"custom-acp": {
			Command: "acp-agent",
		},
	}}
	results := []ReviewResult{
		{
			Agent:      "codex",
			ReviewType: "security",
			Status:     "done",
			Output:     "issue A",
		},
		{
			Agent:      "codex",
			ReviewType: "design",
			Status:     "done",
			Output:     "issue B",
		},
	}

	synthesis, err := Synthesize(t.Context(), results, SynthesizeOpts{
		Agent:        "custom-acp",
		GlobalConfig: cfg,
		HeadSHA:      "abc123",
		GitRef:       "abc123..def456",
	})
	comment := synthesis.GitHubComment
	require.NoError(t, err)
	require.Equal(t, "custom-acp", seenAgent, "resolver agent")
	require.Same(t, cfg, seenCfg, "resolver cfg pointer mismatch")
	assertContains(t, comment, "`file.go:1`: combined\n\n  **Fix:** fix")
}

func TestSuccessfulSynthesisInheritsThreshold(t *testing.T) {
	t.Parallel()
	for _, policy := range []struct {
		name    string
		members []string
		ci      string
		visible bool
	}{
		{"inherited", []string{"medium", "medium"}, "", false},
		{"explicit low", []string{"medium", "medium"}, "low", true},
		{"explicit high", []string{"medium", "medium"}, "high", false},
		{"different thresholds", []string{"high", "medium"}, "", false},
		{"unfiltered member", []string{"medium", ""}, "", true},
	} {
		t.Run(policy.name, func(t *testing.T) {
			a := &structuredBatchAgent{name: "threshold-synthesis", result: []byte(`{"schema_version":4,"summary":"Combined.","verdict":"fail","findings":[{"severity":"low","kind":"improvement","confidence":0.7,"unlikely":false,"problem":"Minor naming issue.","fix":"Rename it.","file":null,"start_line":null,"end_line":null,"sources":[1,2],"source_findings":[{"review":1,"finding":1},{"review":2,"finding":1}]}]}`)}
			agent.RegisterForTest(t, a)
			doc := StructuredReview{SchemaVersion: 2, Summary: "Review complete.", Findings: []StructuredFinding{{Severity: "low", Problem: "Minor naming issue.", Fix: "Rename it."}}}
			results := make([]ReviewResult, len(policy.members))
			for i, threshold := range policy.members {
				results[i] = ReviewResult{Status: ResultDone, Structured: &doc}.ApplyMinSeverity(threshold)
			}
			result, err := Synthesize(t.Context(), results, SynthesizeOpts{Agent: a.Name(), MinSeverity: policy.ci})
			require.NoError(t, err)
			assert.Contains(t, result.Output, "Minor naming issue.")
			if policy.visible {
				assert.Contains(t, result.GitHubComment, "Minor naming issue.")
			} else {
				assert.NotContains(t, result.GitHubComment, "Minor naming issue.")
			}
		})
	}
}

func TestSynthesisDisplayPolicyAcrossOutcomes(t *testing.T) {
	t.Parallel()
	for _, policy := range []struct {
		name    string
		ci      string
		members []string
		visible []string
	}{
		{"inherited mixed", "", []string{"high", "medium"}, []string{"Medium finding.", "High finding."}},
		{"explicit low", "low", []string{"high", "medium"}, []string{"Low finding.", "Medium finding.", "High finding."}},
		{"explicit high", "high", []string{"high", "medium"}, []string{"High finding."}},
		{"unfiltered member", "", []string{"high", ""}, []string{"Low finding.", "Medium finding.", "High finding."}},
	} {
		for _, outcome := range []string{"synthesis", "fallback"} {
			t.Run(policy.name+"/"+outcome, func(t *testing.T) {
				// The document doubles as the synthesis agent's output, so it
				// carries the live schema version.
				doc := StructuredReview{SchemaVersion: storage.StructuredReviewSchemaVersion, Verdict: "fail", Summary: "Review complete.", Findings: []StructuredFinding{
					{Severity: "low", Kind: "defect", Confidence: 0.6, Problem: "Low finding.", Fix: "Fix it.", Sources: []int{1}},
					{Severity: "medium", Kind: "defect", Confidence: 0.7, Problem: "Medium finding.", Fix: "Fix it.", Sources: []int{1}},
					{Severity: "high", Kind: "defect", Confidence: 0.9, Problem: "High finding.", Fix: "Fix it.", Sources: []int{1}},
				}}
				raw, err := json.Marshal(doc)
				require.NoError(t, err)
				for i := range doc.Findings {
					raw = []byte(strings.Replace(string(raw), `"sources":[1]}`, fmt.Sprintf(`"sources":[1],"unlikely":false,"source_findings":[{"review":1,"finding":%d}]}`, i+1), 1))
				}
				a := &structuredBatchAgent{name: "policy-outcome", result: raw}
				if outcome == "fallback" {
					a.err = errors.New("synthesis unavailable")
				}
				agent.RegisterForTest(t, a)
				// Only the high-threshold member reports findings. The second
				// member contributes its policy and a substantive passing review.
				results := []ReviewResult{
					{Status: ResultDone, Output: doc.Markdown(""), Structured: &doc, MinSeverity: policy.members[0]},
					{Status: ResultDone, Output: "No issues found.", MinSeverity: policy.members[1]},
				}
				result, err := Synthesize(t.Context(), results, SynthesizeOpts{Agent: a.Name(), MinSeverity: policy.ci})
				require.NoError(t, err)
				assert := assert.New(t)
				assert.Equal(outcome == "fallback", strings.Contains(result.GitHubComment, "Synthesis unavailable."))
				for _, finding := range doc.Findings {
					assert.Contains(result.Output, finding.Problem)
					assert.Equal(slices.Contains(policy.visible, finding.Problem), strings.Contains(result.GitHubComment, finding.Problem), finding.Problem)
				}
				assert.Equal(policy.members[0], results[0].MinSeverity, "publication must not mutate member policy")
				assert.Len(results[0].Structured.Findings, 3)
			})
		}
	}
}

func TestSynthesizeRawFallbackShowAll(t *testing.T) {
	prose := "### High\nHigh issue.\n\n### Low\nLow issue."
	results := []ReviewResult{
		{Status: ResultDone, Output: prose, Structured: &StructuredReview{Findings: []StructuredFinding{{Severity: "high", Problem: "High issue", Fix: "Fix"}, {Severity: "low", Problem: "Low issue", Fix: "Fix"}}}},
		{Status: ResultDone, Output: "No issues found."},
	}
	result, err := Synthesize(t.Context(), results, SynthesizeOpts{MinSeverity: "medium", ShowAll: true, Agent: "nonexistent-synthesis-agent"})
	require.NoError(t, err)
	assert.Contains(t, result.GitHubComment, "High issue")
	assert.Contains(t, result.GitHubComment, "Low issue")
	assert.NotContains(t, result.GitHubComment, "<details>")
	result, err = Synthesize(t.Context(), results, SynthesizeOpts{MinSeverity: "low", Agent: "nonexistent-synthesis-agent"})
	require.NoError(t, err)
	assert.Contains(t, result.GitHubComment, "Low issue")
	assert.NotContains(t, result.GitHubComment, "<details>")
}

func TestSynthesizeStructuredCommentVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		finding StructuredFinding
		showAll bool
		heading string
	}{
		{"folded", StructuredFinding{Severity: "medium", Problem: "Rare finding", Fix: "Fix", Unlikely: true}, false, "Review Passed"},
		{"show all", StructuredFinding{Severity: "low", Problem: "Low finding", Fix: "Fix"}, true, "Review Complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := StructuredReview{Summary: "Review", Verdict: "fail", Findings: []StructuredFinding{tc.finding}}
			result, err := Synthesize(t.Context(), []ReviewResult{{Status: ResultDone, Structured: &doc, Output: doc.Markdown(""), Verdict: storage.VerdictFail}}, SynthesizeOpts{MinSeverity: "medium", ShowAll: tc.showAll})
			require.NoError(t, err)
			assert.Contains(t, result.GitHubComment, tc.heading)
			assert.Contains(t, result.GitHubComment, tc.finding.Problem)
		})
	}
}
