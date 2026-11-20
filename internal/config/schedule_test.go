package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSchedule(t *testing.T) {
	yes := true
	no := false
	interval := "1h"
	maxFiles := 3
	global := ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &maxFiles, Types: []string{"refactor"}, Paths: []string{"**/*.go"}}
	got, err := ResolveSchedule(ScheduleConfig{}, ScheduleConfig{})
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	got, err = ResolveSchedule(global, ScheduleConfig{})
	require.NoError(t, err)
	assert.True(t, got.Enabled)
	got, err = ResolveSchedule(global, ScheduleConfig{Enabled: &no})
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	got, err = ResolveSchedule(ScheduleConfig{Enabled: &no}, ScheduleConfig{Enabled: &yes, Interval: &interval, MaxFiles: &maxFiles, Types: []string{"refactor"}})
	require.NoError(t, err)
	assert.True(t, got.Enabled)
	got, err = ResolveSchedule(global, ScheduleConfig{Enabled: &yes, Types: []string{"security"}})
	require.NoError(t, err)
	assert.Equal(t, Schedule{Enabled: true, Interval: time.Hour, MaxFiles: 3, Types: []string{"security"}, Paths: []string{"**/*.go"}}, got)
	for _, repo := range []ScheduleConfig{
		{Enabled: &yes},
		{Enabled: &yes, Interval: &interval},
		{Enabled: &yes, Interval: &interval, MaxFiles: &maxFiles},
	} {
		_, err := ResolveSchedule(ScheduleConfig{}, repo)
		assert.Error(t, err)
	}
	for _, repo := range []ScheduleConfig{
		{Enabled: &yes, Interval: new("0s")},
		{Enabled: &yes, MaxFiles: new(0)},
		{Enabled: &yes, Types: []string{"unknown"}},
		{Enabled: &yes, Paths: []string{"["}},
	} {
		err := repo.Validate()
		assert.Error(t, err)
	}
}

func TestAnalyzeDefaultsPrecedence(t *testing.T) {
	global := &Config{DefaultAgent: "default", AnalyzeAgent: "global-analysis", AnalyzeModel: "global-model", AnalyzeReasoning: "low", Analyze: map[string]AnalyzeConfig{"refactor": {Agent: "global-type"}}}
	repo := &RepoConfig{AnalyzeAgent: "repo-analysis", AnalyzeModel: "repo-model", AnalyzeReasoning: "high", Analyze: map[string]AnalyzeConfig{"refactor": {Agent: "repo-type"}}}
	got, err := ResolveAnalyzeConfigFromConfig("", "", "", repo, global, "refactor", "review", "")
	require.NoError(t, err)
	assert.Equal(t, "repo-type", got.Agent)
	assert.Equal(t, "repo-model", got.Model)
	assert.Equal(t, "high", got.Reasoning)
	got, err = ResolveAnalyzeConfigFromConfig("", "", "", nil, global, "refactor", "review", "")
	require.NoError(t, err)
	assert.Equal(t, "global-type", got.Agent)
	assert.Equal(t, "global-model", got.Model)
	assert.Equal(t, "low", got.Reasoning)
	got, err = ResolveAnalyzeConfigFromConfig("cli-agent", "cli-model", "medium", repo, global, "refactor", "review", "")
	require.NoError(t, err)
	assert.Equal(t, AnalyzeConfig{Agent: "cli-agent", Model: "cli-model", Reasoning: "medium"}, got)
	assert.Error(t, (&Config{AnalyzeReasoning: "invalid"}).Validate())
	assert.Error(t, (&RepoConfig{AnalyzeReasoning: "invalid"}).Validate())
}
