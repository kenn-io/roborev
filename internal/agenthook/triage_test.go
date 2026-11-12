package agenthook

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agentguide"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestIsBuiltinInstructionLegacy(t *testing.T) {
	for _, s := range legacyDefaultInstructions {
		assert.True(t, IsBuiltinInstruction(s), s)
	}
	assert.True(t, IsBuiltinInstruction(""))
	assert.True(t, IsBuiltinInstruction(DefaultInstruction))
	assert.False(t, IsBuiltinInstruction("Resolve open roborev findings now."))
	assert.Equal(t, DefaultInstruction, effectiveInstruction("Invoke the $roborev-fix skill now."))
	assert.Empty(t, customInstruction(DefaultInstruction))
	assert.Equal(t, "Ask me.", effectiveInstruction("  Ask me.  "))
}

func TestIsBuiltinInstructionLegacyWhitespaceProperty(t *testing.T) {
	property := func(textIndex, whitespaceIndex uint8) bool {
		text := legacyDefaultInstructions[int(textIndex)%len(legacyDefaultInstructions)]
		whitespace := []string{" ", "\n", "\r\n", "\t", "  \n  "}
		ws := whitespace[int(whitespaceIndex)%len(whitespace)]
		rewrapped := ws + strings.Join(strings.Fields(text), ws) + ws
		return assert.True(t, IsBuiltinInstruction(rewrapped), "instruction=%q whitespace=%q", text, ws)
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestBuildPromptReasonLegacyUsesCurrentInstruction(t *testing.T) {
	reason := buildStopReason(Request{Instruction: "Invoke the $roborev-fix skill now."}, 5, nil)
	assert.Contains(t, reason, DefaultInstruction)
	assert.NotContains(t, reason, "Invoke the $roborev-fix skill now.")
}

func TestDeferredReminderWithLegacyInstructionRendersCurrentDefault(t *testing.T) {
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "initial")
	count := 1
	store := &StateStore{reviews: newDeferredReminderSource(t, repo.Path(), &count), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	pending := PendingReminder{TriggeredBy: "failed_reviews", Instruction: legacyDefaultInstructions[0], TrackedRepoRoot: repo.Path(), WorktreeRoot: repo.Path(), Branch: "main", LineageKey: "repo", FailedReviewCount: 1}
	state := SessionState{}
	queuePendingReminder(&state, pending)
	store.sessions["session-1"] = state
	seedOwnedCommitFixtures(t, store, []string{"session-1"}, repo.Path())
	resp, err := store.Record(Request{Event: Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: "Stop"}})
	require.NoError(t, err)
	require.True(t, resp.Triggered)
	assert.Contains(t, resp.Reason, DefaultInstruction)
	assert.NotContains(t, resp.Reason, legacyDefaultInstructions[0])
}

func TestDefaultInstructionDOTSemantics(t *testing.T) {
	graph, err := agentguide.LintDOT(DefaultInstruction)
	require.NoError(t, err)
	assert.Equal(t, "roborev_triage", graph.Name)
	assert.Equal(t, "hold", graph.Out("start")[0].To, "pause/wait guard precedes instructions to work")
	assert.Empty(t, graph.Out("resume"))
	assert.Len(t, graph.Out("triage"), 4)
	body, err := agentguide.ExtractDOT(DefaultInstruction)
	require.NoError(t, err)
	err = agentguide.CanonWithGraphviz(t.Context(), body)
	if errors.Is(err, agentguide.ErrGraphvizUnavailable) {
		t.Skip("graphviz not installed")
	}
	require.NoError(t, err)
}

var updateGolden = flag.Bool("update-golden", false, "regenerate agent hook golden files")

func TestDefaultStopReasonGolden(t *testing.T) {
	reason := buildStopReason(Request{}, 5, reviewIDSet{1201: {}, 1204: {}, 1207: {}})
	path := filepath.Join("testdata", "golden", "stop_reason_default.golden")
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(reason), 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), reason)
}

func TestReasonsCloseFenceOnOwnLine(t *testing.T) {
	ids := reviewIDSet{7: {}}
	req := Request{}
	for name, reason := range map[string]string{
		"stop":     buildStopReason(req, 5, ids),
		"commit":   buildCommitReason(req, 2, "/workspace/demo", ids),
		"failed":   buildFailedReviewReason(req, SessionState{FailedReviewCount: 1}, ids),
		"deferred": deferredReminderReason(buildStopReason(req, 5, ids), "/workspace/demo"),
		"injected": buildUserPromptReason(req, hookScope{TrackedRepoRoot: "/workspace/demo"}, []storage.ReviewJob{{ID: 7, GitRef: "abc"}}, ids),
	} {
		t.Run(name, func(t *testing.T) {
			for line := range strings.SplitSeq(reason, "\n") {
				if strings.Contains(line, "~~~") {
					assert.True(t, slices.Contains([]string{"~~~dot", "~~~"}, line), line)
				}
			}
			_, err := agentguide.LintDOT(reason)
			require.NoError(t, err)
		})
	}
}

func TestBuildPromptReasonCustomInstructionWithFenceIsVerbatim(t *testing.T) {
	custom := "Do this:\n\n~~~dot\ndigraph mine { a -> b; }\n~~~"
	assert.Equal(t, reminderPreamble+"\n\n"+custom+"\n\n5 Stop hooks reached.", buildStopReason(Request{Instruction: custom}, 5, nil))
}
