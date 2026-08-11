package tui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestTUIActiveSnoozeRequiresExactFilteredCheckout(t *testing.T) {
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	base := model{
		activeRepoFilter:   []string{"/repos/roborev"},
		activeBranchFilter: "feature/status",
		cwdRepoRoot:        "/repos/roborev",
		cwdWorktreePath:    "/worktrees/status",
		cwdBranch:          "feature/status",
		status: storage.DaemonStatus{ActiveSnoozes: []storage.AgentHookSnooze{{
			RepoPath:     "/repos/roborev",
			WorktreePath: "/worktrees/status",
			Branch:       "feature/status",
			SnoozedUntil: until,
		}}},
	}

	tests := []struct {
		name   string
		mutate func(*model)
		want   bool
	}{
		{name: "exact scope", want: true},
		{name: "broad repo", mutate: func(m *model) { m.activeRepoFilter = nil }},
		{name: "different repo", mutate: func(m *model) {
			m.activeRepoFilter = []string{"/repos/other"}
		}},
		{name: "aggregate repos", mutate: func(m *model) {
			m.activeRepoFilter = []string{"/repos/roborev", "/repos/mirror"}
		}},
		{name: "broad branch", mutate: func(m *model) { m.activeBranchFilter = "" }},
		{name: "different branch", mutate: func(m *model) {
			m.activeBranchFilter = "main"
		}},
		{name: "sibling worktree", mutate: func(m *model) {
			m.cwdWorktreePath = "/worktrees/sibling"
		}},
		{name: "expired", mutate: func(m *model) {
			m.status.ActiveSnoozes[0].SnoozedUntil = now
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base
			m.activeRepoFilter = append([]string(nil), base.activeRepoFilter...)
			m.status.ActiveSnoozes = append(
				[]storage.AgentHookSnooze(nil), base.status.ActiveSnoozes...,
			)
			if tt.mutate != nil {
				tt.mutate(&m)
			}
			assert.Equal(t, tt.want, m.activeSnooze(now) != nil)
		})
	}
}

func TestDetectCwdRepoContextPreservesLinkedWorktree(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	worktree := filepath.Join(t.TempDir(), "status-worktree")
	repo.Run("worktree", "add", "-b", "feature/status", worktree)

	repoRoot, _, worktreePath, branch := detectCwdRepoContext(
		context.Background(), worktree,
	)
	expectedRepoRoot, err := filepath.EvalSymlinks(repo.Path())
	require.NoError(t, err)
	expectedWorktree, err := filepath.EvalSymlinks(worktree)
	require.NoError(t, err)
	assert.Equal(t, filepath.ToSlash(expectedRepoRoot), repoRoot)
	assert.Equal(t, filepath.ToSlash(expectedWorktree), worktreePath)
	assert.Equal(t, "feature/status", branch)
}

func TestTUIQueueTitleShowsExactSnooze(t *testing.T) {
	until := time.Now().Add(time.Hour)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.activeRepoFilter = []string{"/repos/roborev"}
	m.activeBranchFilter = "feature/status"
	m.cwdRepoRoot = "/repos/roborev"
	m.cwdWorktreePath = "/worktrees/status"
	m.cwdBranch = "feature/status"
	m.status.ActiveSnoozes = []storage.AgentHookSnooze{{
		RepoPath:     "/repos/roborev",
		WorktreePath: "/worktrees/status",
		Branch:       "feature/status",
		SnoozedUntil: until,
	}}

	out := stripTestANSI(m.renderQueueView())
	assert.Contains(t, out,
		"[SNOOZED until "+until.Local().Format("Jan 02 15:04")+"]")

	m.height = 10
	compact := stripTestANSI(m.renderQueueView())
	assert.Contains(t, compact, "[SNOOZED")

	m.activeBranchFilter = "main"
	differentBranch := stripTestANSI(m.renderQueueView())
	assert.NotContains(t, differentBranch, "[SNOOZED")
}
