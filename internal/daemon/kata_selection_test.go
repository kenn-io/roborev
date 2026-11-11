package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataSelectionCoalescesAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Integrations: config.IntegrationsConfig{Kata: config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test", DefaultProject: "reviews"}}}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews", Labels: []string{"b", "a"}}, {Type: "kata", Event: "review.*", Findings: "comments", Project: "reviews", Labels: []string{"a", "b"}}}
	job := &storage.ReviewJob{JobType: storage.JobTypeReview}
	selection, err := selectKataPublication(t.Context(), cfg, job, "")
	require.NoError(t, err)
	require.NotNil(t, selection)
	assert.Equal(t, "reviews", selection.seed.Project)
	cfg.Hooks[1].Findings = "summary"
	selection, err = selectKataPublication(t.Context(), cfg, job, "")
	require.Error(t, err)
	assert.Nil(t, selection)
	job.Source = storage.JobSourceCI
	selection, err = selectKataPublication(t.Context(), cfg, job, "")
	require.NoError(t, err)
	assert.Nil(t, selection)
}

func TestKataHookFindingsValidation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Hooks = []config.HookConfig{{Type: "kata", Findings: "comments"}}
	require.NoError(t, cfg.Validate())
	cfg.Hooks[0].Findings = "summary"
	require.NoError(t, cfg.Validate())
	cfg.Hooks[0].Findings = "unexpected"
	require.ErrorContains(t, cfg.Validate(), "findings")
	cfg.Hooks[0] = config.HookConfig{Type: "webhook", Findings: "comments"}
	require.ErrorContains(t, cfg.Validate(), "findings")
	repo := &config.RepoConfig{Hooks: []config.HookConfig{{Type: "kata", Findings: "unexpected"}}}
	require.ErrorContains(t, repo.Validate(), "findings")
}
