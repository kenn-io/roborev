package agenthook

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/testutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"hegel.dev/go/hegel"
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
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.SampledFrom(legacyDefaultInstructions))
		ws := hegel.Draw(ht, hegel.SampledFrom([]string{" ", "\n", "\r\n", "\t", "  \n  "}))
		rewrapped := ws + strings.Join(strings.Fields(text), ws) + ws
		assert.True(ht, IsBuiltinInstruction(rewrapped))
	})
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
	store := &StateStore{reviews: newDeferredReminderSource(repo.Path(), &count), path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	pending := PendingReminder{TriggeredBy: "failed_reviews", Instruction: legacyDefaultInstructions[0], TrackedRepoRoot: repo.Path(), WorktreeRoot: repo.Path(), Branch: "main", LineageKey: "repo", FailedReviewCount: 1}
	state := SessionState{}
	queuePendingReminder(&state, pending)
	store.sessions["session-1"] = state
	resp, err := store.Record(Request{Event: Input{SessionID: "session-1", CWD: repo.Path(), HookEventName: "Stop"}})
	require.NoError(t, err)
	require.True(t, resp.Triggered)
	assert.Contains(t, resp.Reason, DefaultInstruction)
	assert.NotContains(t, resp.Reason, legacyDefaultInstructions[0])
}
