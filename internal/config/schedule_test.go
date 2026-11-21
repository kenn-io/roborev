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
	invalid := global
	invalid.MaxFiles = new(-1)
	_, err = ResolveSchedule(invalid, ScheduleConfig{})
	require.Error(t, err)
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
		{Enabled: &yes, MaxFiles: &maxFiles, Types: []string{"refactor"}},
		{Enabled: &yes, Interval: &interval, Types: []string{"refactor"}},
		{Enabled: &yes, Interval: &interval, MaxFiles: &maxFiles},
	} {
		_, err := ResolveSchedule(ScheduleConfig{}, repo)
		require.Error(t, err)
	}
	for _, repo := range []ScheduleConfig{
		{Enabled: &yes, Interval: new("0s")},
		{Enabled: &yes, MaxFiles: new(0)},
		{Enabled: &yes, Types: []string{"unknown"}},
		{Enabled: &yes, Paths: []string{"["}},
	} {
		_, err := ResolveSchedule(global, repo)
		require.Error(t, err)
	}
}
