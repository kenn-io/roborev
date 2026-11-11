package daemon

import (
	"os"
	"path/filepath"
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

func TestKataSelectionEligibility(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "summary", Project: "reviews", Branches: []string{"main"}}}
	cases := []struct {
		name     string
		job      storage.ReviewJob
		eligible bool
	}{
		{"review", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main"}, true},
		{"range", storage.ReviewJob{JobType: storage.JobTypeRange, Branch: "main"}, true},
		{"dirty", storage.ReviewJob{JobType: storage.JobTypeDirty, Branch: "main"}, true},
		{"synthesis", storage.ReviewJob{JobType: storage.JobTypeSynthesis, Branch: "main"}, true},
		{"CI", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", Source: storage.JobSourceCI}, false},
		{"CI base", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", CIBaseBranch: "main"}, false},
		{"member", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", PanelRole: storage.PanelRoleMember}, false},
		{"rebuttal", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", Source: storage.JobSourceRebuttal}, false},
		{"goal", storage.ReviewJob{JobType: storage.JobTypeGoalReview, Branch: "main"}, false},
		{"task", storage.ReviewJob{JobType: storage.JobTypeTask, Branch: "main"}, false},
		{"compact", storage.ReviewJob{JobType: storage.JobTypeCompact, Branch: "main"}, false},
		{"branch", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "other"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := selectKataPublication(t.Context(), cfg, &tc.job, "")
			require.NoError(t, err)
			assert.Equal(t, tc.eligible, selected != nil)
		})
	}
}

func TestKataSelectionSuppressesSuppliedRepoHooks(t *testing.T) {
	dir := t.TempDir()
	text := "[[hooks]]\ntype = \"kata\"\nevent = \"review.completed\"\nfindings = \"comments\"\nproject = \"repo\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(text), 0o600))
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "global"}}
	job := &storage.ReviewJob{JobType: storage.JobTypeReview}
	_, err := selectKataPublication(t.Context(), cfg, job, dir)
	require.ErrorContains(t, err, "conflicting")
	job.Source = storage.JobSourceReviewPrompt
	job.PanelMemberConfigJSON = "supplied"
	selected, err := selectKataPublication(t.Context(), cfg, job, dir)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "global", selected.seed.Project)
}
