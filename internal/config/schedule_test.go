package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeScheduleUsesExplicitRepositoryFields(t *testing.T) {
	enabled := true
	global := ScheduleConfig{Enabled: &enabled, Interval: "1h", Types: []string{"refactor"}, Paths: []string{"internal/"}, MaxFiles: 4, Agent: "global", Model: "global-model", Reasoning: "standard"}
	repo := ScheduleConfig{Interval: "15m", Paths: []string{}, MaxFiles: 2, Agent: "repo"}
	got := MergeSchedule(global, repo, map[string]any{"schedule": map[string]any{"interval": "15m", "paths": []any{}, "max_files": int64(2), "agent": "repo"}})
	assert.True(t, got.Enabled)
	assert.Equal(t, 15*time.Minute, got.Interval)
	assert.Empty(t, got.Paths)
	assert.Equal(t, 2, got.MaxFiles)
	assert.Equal(t, "repo", got.Agent)
	assert.Equal(t, "global-model", got.Model)
}

func TestScheduleValidation(t *testing.T) {
	enabled := true
	cfg := ScheduleConfig{Enabled: &enabled, Interval: "1h", MaxFiles: 0}
	require.Error(t, cfg.Validate(true))
	cfg.MaxFiles = 1
	cfg.Types = []string{"refactor"}
	require.NoError(t, cfg.Validate(true))
	cfg.Interval = "invalid"
	require.Error(t, cfg.Validate(true))
}

func TestResolveScheduledAnalyzeConfigUsesScheduleWorkflow(t *testing.T) {
	repo := &RepoConfig{Schedule: ScheduleConfig{Agent: "repo-schedule", Model: "repo-model", Reasoning: "high"}}
	global := &Config{DefaultAgent: "global", DefaultModel: "global-model"}
	got, err := ResolveScheduledAnalyzeConfigFromConfig(repo, global, "refactor")
	require.NoError(t, err)
	assert.Equal(t, "repo-schedule", got.Agent)
	assert.Equal(t, "repo-model", got.Model)
	assert.Equal(t, "high", got.Reasoning)
}

func TestResolveScheduledAnalyzeConfigUsesGenericAndSecurityWorkflows(t *testing.T) {
	global := &Config{
		ReviewAgent:   "generic-review",
		SecurityAgent: "security-review",
		ReviewModel:   "generic-model",
		SecurityModel: "security-model",
	}

	got, err := ResolveScheduledAnalyzeConfigFromConfig(nil, global, "complexity")
	require.NoError(t, err)
	assert.Equal(t, "generic-review", got.Agent)
	assert.Equal(t, "generic-model", got.Model)

	got, err = ResolveScheduledAnalyzeConfigFromConfig(nil, global, "security")
	require.NoError(t, err)
	assert.Equal(t, "security-review", got.Agent)
	assert.Equal(t, "security-model", got.Model)
}

func TestLoadSchedulePreservesRepositoryOptInAndEmptyPaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte("[schedule]\nenabled = true\npaths = []\n"), 0o600))
	cfg, raw, err := LoadRepoConfigWithRaw(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Schedule.Enabled)
	assert.True(t, *cfg.Schedule.Enabled)
	assert.Empty(t, cfg.Schedule.Paths)
	_, ok := scheduleKey(raw, "paths")
	assert.True(t, ok, "the raw TOML map must retain an explicit empty paths value")
}

func TestScheduleEnablementRequiresExplicitRepositoryOptIn(t *testing.T) {
	enabled := true
	global := ScheduleConfig{Enabled: &enabled}
	repo := RepoConfig{}
	assert.Nil(t, repo.Schedule.Enabled)
	repoEnabled := true
	repo.Schedule.Enabled = &repoEnabled
	assert.True(t, MergeSchedule(global, repo.Schedule, map[string]any{"schedule": map[string]any{"enabled": true}}).Enabled)
	globalEnabled := false
	assert.False(t, MergeSchedule(ScheduleConfig{Enabled: &globalEnabled}, repo.Schedule, map[string]any{"schedule": map[string]any{"enabled": true}}).Enabled)
	assert.NotNil(t, global.Enabled)
}

func TestScheduleValidationUsesAllAnalysisTypes(t *testing.T) {
	enabled := true
	cfg := ScheduleConfig{Enabled: &enabled, Interval: "1h", Types: []string{"security"}, MaxFiles: 1}
	require.NoError(t, cfg.Validate(true))
}

func TestScheduleValidationRejectsIncompleteGlobalPolicy(t *testing.T) {
	enabled := true
	cfg := ScheduleConfig{Enabled: &enabled, Interval: "1h", MaxFiles: 1}
	require.Error(t, cfg.Validate(true))
	cfg.Types = []string{"refactor"}
	require.NoError(t, cfg.Validate(true))
	cfg.Paths = []string{"../outside"}
	require.Error(t, cfg.Validate(true))
	cfg.Paths = []string{"C:/repo/internal"}
	require.Error(t, cfg.Validate(true))
}
