package config

import (
	"fmt"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.kenn.io/roborev/internal/prompt/analyze"
)

// ScheduleConfig controls periodic analysis. Repository values override global values.
type ScheduleConfig struct {
	Enabled  *bool    `toml:"enabled"`
	Interval *string  `toml:"interval"`
	Types    []string `toml:"types"`
	Paths    []string `toml:"paths"`
	MaxFiles *int     `toml:"max_files"`
}

type Schedule struct {
	Enabled      bool
	Interval     time.Duration
	Types, Paths []string
	MaxFiles     int
}

func (s ScheduleConfig) Validate() error {
	if s.Interval != nil {
		duration, err := time.ParseDuration(*s.Interval)
		if err != nil || duration <= 0 {
			return fmt.Errorf("schedule.interval must be a positive duration")
		}
	}
	if s.MaxFiles != nil && *s.MaxFiles <= 0 {
		return fmt.Errorf("schedule.max_files must be positive")
	}
	seen := map[string]bool{}
	for _, typ := range s.Types {
		if analyze.GetType(typ) == nil || seen[typ] {
			return fmt.Errorf("invalid schedule type %q", typ)
		}
		seen[typ] = true
	}
	for _, pattern := range s.Paths {
		if !doublestar.ValidatePattern(pattern) {
			return fmt.Errorf("invalid schedule path %q", pattern)
		}
	}
	return nil
}

func ResolveSchedule(global ScheduleConfig, repo ScheduleConfig) (Schedule, error) {
	enabled := repo.Enabled
	if enabled == nil {
		enabled = global.Enabled
	}
	if enabled == nil || !*enabled {
		return Schedule{}, nil
	}
	merged := global
	if repo.Interval != nil {
		merged.Interval = repo.Interval
	}
	if repo.Types != nil {
		merged.Types = repo.Types
	}
	if repo.Paths != nil {
		merged.Paths = repo.Paths
	}
	if repo.MaxFiles != nil {
		merged.MaxFiles = repo.MaxFiles
	}
	if merged.Interval == nil {
		return Schedule{}, fmt.Errorf("schedule.interval is required")
	}
	interval, err := time.ParseDuration(*merged.Interval)
	if err != nil {
		return Schedule{}, fmt.Errorf("schedule.interval must be a positive duration")
	}
	if merged.MaxFiles == nil {
		return Schedule{}, fmt.Errorf("schedule.max_files must be positive")
	}
	if len(merged.Types) == 0 {
		return Schedule{}, fmt.Errorf("schedule.types must contain an analysis type")
	}
	return Schedule{Enabled: true, Interval: interval, Types: merged.Types, Paths: merged.Paths, MaxFiles: *merged.MaxFiles}, nil
}
