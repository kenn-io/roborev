package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ScheduleConfig controls daemon analysis scheduling. A nil Enabled value in
// repository configuration means that the repository has not opted in.
type ScheduleConfig struct {
	Enabled   *bool    `toml:"enabled"`
	Interval  string   `toml:"interval"`
	Types     []string `toml:"types"`
	Paths     []string `toml:"paths"`
	MaxFiles  int      `toml:"max_files"`
	Agent     string   `toml:"agent"`
	Model     string   `toml:"model"`
	Reasoning string   `toml:"reasoning"`
}

type EffectiveSchedule struct {
	Enabled   bool
	Interval  time.Duration
	Types     []string
	Paths     []string
	MaxFiles  int
	Agent     string
	Model     string
	Reasoning string
}

func (c ScheduleConfig) Validate(global bool) error {
	enabled := c.Enabled != nil && *c.Enabled
	if c.Interval != "" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil || d <= 0 {
			return fmt.Errorf("schedule.interval must be a positive duration")
		}
	}
	if global && enabled {
		if c.Interval == "" {
			return fmt.Errorf("schedule.interval is required when scheduling is enabled")
		}
		if len(c.Types) == 0 {
			return fmt.Errorf("schedule.types must not be empty when scheduling is enabled")
		}
		if c.MaxFiles <= 0 {
			return fmt.Errorf("schedule.max_files must be positive when scheduling is enabled")
		}
	}
	if c.Reasoning != "" {
		if _, err := NormalizeReasoning(c.Reasoning); err != nil {
			return fmt.Errorf("schedule.reasoning: %w", err)
		}
	}
	for _, typ := range c.Types {
		switch strings.TrimSpace(typ) {
		case "duplication", "refactor", "complexity", "test-fixtures", "api-design", "dead-code", "architecture":
		default:
			return fmt.Errorf("schedule.types contains unknown analysis type %q", typ)
		}
	}
	for _, path := range c.Paths {
		normalized := strings.TrimSuffix(filepath.ToSlash(path), "/")
		clean := filepath.ToSlash(filepath.Clean(normalized))
		if normalized == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || clean != normalized {
			return fmt.Errorf("schedule.paths contains invalid repository path %q", path)
		}
	}
	return nil
}

func scheduleKey(raw map[string]any, key string) (any, bool) {
	table, ok := raw["schedule"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := table[key]
	return v, ok
}

func MergeSchedule(global ScheduleConfig, repo ScheduleConfig, raw map[string]any) EffectiveSchedule {
	result := EffectiveSchedule{Enabled: global.Enabled != nil && *global.Enabled, MaxFiles: global.MaxFiles, Paths: append([]string(nil), global.Paths...), Types: append([]string(nil), global.Types...), Agent: global.Agent, Model: global.Model, Reasoning: global.Reasoning}
	if global.Interval != "" {
		result.Interval, _ = time.ParseDuration(global.Interval)
	}
	if v, ok := scheduleKey(raw, "enabled"); ok {
		if enabled, ok := v.(bool); ok {
			result.Enabled = enabled
		}
	}
	if _, ok := scheduleKey(raw, "interval"); ok {
		result.Interval, _ = time.ParseDuration(repo.Interval)
	}
	if _, ok := scheduleKey(raw, "types"); ok {
		result.Types = append([]string(nil), repo.Types...)
	}
	if _, ok := scheduleKey(raw, "paths"); ok {
		result.Paths = append([]string(nil), repo.Paths...)
	}
	if _, ok := scheduleKey(raw, "max_files"); ok {
		result.MaxFiles = repo.MaxFiles
	}
	if _, ok := scheduleKey(raw, "agent"); ok {
		result.Agent = repo.Agent
	}
	if _, ok := scheduleKey(raw, "model"); ok {
		result.Model = repo.Model
	}
	if _, ok := scheduleKey(raw, "reasoning"); ok {
		result.Reasoning = repo.Reasoning
	}
	return result
}

func (c Config) ValidateSchedule() error { return c.Schedule.Validate(true) }

func (c RepoConfig) ValidateSchedule() error { return c.Schedule.Validate(false) }

func (s EffectiveSchedule) EnabledForRepo(repo *RepoConfig, raw map[string]any) bool {
	return s.Enabled && repo != nil && repo.Schedule.Enabled != nil && *repo.Schedule.Enabled
}
