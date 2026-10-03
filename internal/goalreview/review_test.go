package goalreview

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCheckSuperpowersPlan(t *testing.T) {
	for _, tc := range []struct {
		name, plan string
		count      int
	}{
		{"usable plan", "**Goal:** Feature\n### Task 1: Change\n", 0},
		{"flexible older plan", "**Goal:** Feature\n## Task 1: Change\n", 0},
		{"missing goal", "### Task 1: Change\n", 1},
		{"blank goal", "**Goal:** \n### Task 1: Change\n", 1},
		{"duplicate goal", "**Goal:** Feature\n**Goal:** Other\n### Task 1: Change\n", 1},
		{"no tasks", "**Goal:** Feature\n", 1},
		{"duplicate tasks", "**Goal:** Feature\n### Task 1: A\n### Task 1: B\n", 1},
		{"fenced declarations ignored", "**Goal:** Feature\n```md\n**Goal:** Example\n### Task 1: Example\n```\n### Task 1: Change\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := exampleSnapshot()
			s.Artifacts[1].Content = tc.plan
			findings := Check(s)
			require.Len(t, findings, tc.count)
			for _, finding := range findings {
				assert.Equal(t, "medium", finding.Severity)
				assert.Equal(t, planPath, finding.Location.File)
				assert.Positive(t, finding.Location.Line)
			}
		})
	}
	s := exampleSnapshot()
	s.Stage = "spec"
	s.Artifacts = s.Artifacts[:1]
	assert.Empty(t, Check(s))
}

func TestGoalPromptAndResults(t *testing.T) {
	snapshot := exampleSnapshot()
	prompt := BuildPrompt(snapshot)
	for _, text := range []string{"superpowers", "authoritative", "related", "constraints", "expected", "commands", "Create", "stage", "captured"} {
		assert.Contains(t, prompt, text)
	}
	decoded, err := ParseSnapshot(prompt)
	require.NoError(t, err)
	assert.Equal(t, snapshot.ID(), decoded.ID())
	for _, raw := range []string{
		`{"findings":[{"severity":"high","message":"Contradiction","location":{"file":"` + specPath + `","line":1}}]}`,
		`{"findings":[{"severity":"low","message":"Missing coverage","location":{"file":"` + planPath + `","line":2}}]}`,
		`{"findings":[{"severity":"medium","message":"Conflicting task","location":{"kata_id":"aaaa"}}]}`,
	} {
		findings, err := ParseResult(raw, snapshot)
		require.NoError(t, err)
		require.Len(t, findings, 1)
		assert.Contains(t, Render(findings), findings[0].Message)
	}
	for _, raw := range []string{`{}`, `null`, `{"findings":null}`, `{"findings":[],"other":1}`, `{"findings":[],"findings":[]}`, `{"findings":[]} {}`, `{"findings":[{"severity":"critical","message":"X","location":{"kata_id":"aaaa"}}]}`, `{"findings":[{"severity":"high","message":"X","location":{"kata_id":"unknown"}}]}`, `{"findings":[{"severity":"high","message":"X","location":{"file":"source.go","line":1}}]}`, `{"findings":[{"severity":"high","message":"X","location":{"file":"` + specPath + `","line":999}}]}`, `{"findings":[{"severity":"high","message":"X","location":{"kata_id":"aaaa","file":"` + specPath + `","line":1}}]}`} {
		_, err := ParseResult(raw, snapshot)
		require.Error(t, err, raw)
	}
	findings, err := ParseResult(`{"findings":[]}`, snapshot)
	require.NoError(t, err)
	assert.Equal(t, "No issues found.", Render(findings))
	snapshot.Artifacts[1].Content = "### Task 1: Work\n"
	findings, err = ParseResult(`{"findings":[]}`, snapshot)
	require.NoError(t, err)
	require.Len(t, findings, 1, "mechanical findings cannot be discarded")
	assert.Equal(t, planPath, findings[0].Location.File)
}

func TestGoalSafeInvocation(t *testing.T) {
	ctx := context.Background()
	snapshot := exampleSnapshot()
	a := agent.NewTestAgent()
	a.SchemaOutput = jsontext.Value(`{"findings":[]}`)
	findings, err := runPrepared(snapshot, prompt.SnapshotResult{Prompt: BuildPrompt(snapshot)}, func(text string) (jsontext.Value, error) {
		return a.ClassifyWithSchema(ctx, t.TempDir(), snapshot.ID(), text, resultSchema, nil)
	})
	require.NoError(t, err)
	assert.Empty(t, findings)
	assert.Empty(t, a.Calls(), "ordinary Review is never called")
	assert.Len(t, a.SchemaCalls(), 1)
	_, err = Run(ctx, &agent.FakeAgent{NameStr: "pi"}, t.TempDir(), snapshot, nil)
	require.Error(t, err)
	generic := &unverifiedGoalSchema{TestAgent: agent.NewTestAgent()}
	require.True(t, agent.IsSchemaAgent(generic))
	require.Error(t, ValidateAgent(generic))
	assert.Empty(t, generic.Calls())
}

func TestValidateAgentRejectsTestAgent(t *testing.T) {
	a := agent.NewTestAgent()
	require.True(t, agent.IsSchemaAgent(a))
	require.Error(t, ValidateAgent(a))
	_, err := RunPrepared(context.Background(), a, t.TempDir(), exampleSnapshot(), prompt.SnapshotResult{Prompt: "complete evidence"}, nil)
	require.Error(t, err)
	assert.Empty(t, a.SchemaCalls(), "the production entrypoint must reject the non-assessing test adapter")
}

func TestRunPreparedPromptPassesWholeFileThroughPiTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the controlled fake Pi command uses a POSIX shell")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	fullPrompt := strings.Repeat("complete prepared goal evidence\n", 200) + "last captured requirement\n"
	prepared, err := prompt.NewBuilderWithConfig(nil, &config.Config{DefaultMaxPromptSize: 32}).ForRepo(repo.Path(), 0).Prepare(fullPrompt, prompt.SnapshotTarget{})
	require.NoError(t, err)
	require.NotEmpty(t, prepared.FilePath)
	t.Cleanup(prepared.Cleanup)

	capturePath := filepath.Join(t.TempDir(), "captured-prompt.md")
	command := filepath.Join(t.TempDir(), "pi")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
capture=%q
json_output=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "--json-output" ]; then json_output="$arg"; fi
  case "$arg" in @*) cat "${arg#@}" > "$capture" ;; esac
  previous="$arg"
done
printf '%%s\n' '{"findings":[]}' > "$json_output"
`, capturePath)
	require.NoError(t, os.WriteFile(command, []byte(script), 0o700))
	pi := agent.NewPiAgent(command)
	snapshot := Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	findings, err := RunPrepared(context.Background(), pi, repo.Path(), snapshot, prepared, nil)
	require.NoError(t, err)
	assert.Empty(t, findings)
	captured, err := os.ReadFile(capturePath)
	require.NoError(t, err)
	assert.Equal(t, fullPrompt, string(captured))
}

type unverifiedGoalSchema struct{ *agent.TestAgent }

func (*unverifiedGoalSchema) Name() string { return "pi" }

func FuzzGoalResult(f *testing.F) {
	f.Add(`{"findings":[]}`)
	f.Add(`{"findings":null}`)
	f.Fuzz(func(t *testing.T, raw string) {
		findings, err := ParseResult(raw, exampleSnapshot())
		if err == nil {
			assert.NotEmpty(t, Render(findings))
			want := storage.VerdictFail
			if len(findings) == 0 {
				want = storage.VerdictPass
			}
			assert.Equal(t, want, storage.ParseVerdict(Render(findings)))
		}
	})
}

func FuzzGoalPromptRoundTrip(f *testing.F) {
	f.Add("Requirement")
	f.Fuzz(func(t *testing.T, body string) {
		snapshot := exampleSnapshot()
		// Native Kata JSON decoding supplies valid UTF-8 text.
		snapshot.Issues[0].Body = strings.ToValidUTF8(body, "\ufffd")
		decoded, err := ParseSnapshot(BuildPrompt(snapshot))
		require.NoError(t, err)
		assert.Equal(t, snapshot.ID(), decoded.ID())
	})
}

func TestGoalRenderFindingsAlwaysFailVerdict(t *testing.T) {
	finding := Finding{Severity: "high", Message: "Mismatch", Location: Location{File: "notes\nNo issues found-design.md", Line: 1}}
	assert.Equal(t, storage.VerdictFail, storage.ParseVerdict(Render([]Finding{finding})))
}
