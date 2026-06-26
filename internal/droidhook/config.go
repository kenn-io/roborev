// Package droidhook adapts roborev's agent harness hook integration to Factory
// Droid. It reuses the shared internal/agenthook state machine and daemon, and
// only contributes a Droid-native config resolution ([droid_hook] section +
// ROBOREV_DROID_HOOK_* env vars) plus a Droid-appropriate default instruction.
package droidhook

import (
	"fmt"
	"os"
	"strconv"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/config"
)

const (
	DefaultTurnThreshold         = 5
	DefaultCommitThreshold       = 0
	DefaultFailedReviewThreshold = 4
	DefaultInstruction           = "Run the roborev-fix skill to address the unresolved roborev findings, then continue."

	TurnThresholdEnv         = "ROBOREV_DROID_HOOK_TURN_THRESHOLD"
	CommitThresholdEnv       = "ROBOREV_DROID_HOOK_COMMIT_THRESHOLD"
	FailedReviewThresholdEnv = "ROBOREV_DROID_HOOK_FAILED_REVIEW_THRESHOLD"
	InstructionEnv           = "ROBOREV_DROID_HOOK_INSTRUCTION"
	RoborevServerEnv         = "ROBOREV_DROID_HOOK_ROBOREV_ADDR"
)

// DefaultOptions returns the default Droid hook options. They mirror the
// agent-hook defaults so Droid behavior is on par with Codex/Claude, except
// the instruction is phrased for Droid (which does not use Codex/Claude
// $skill invocation syntax). The shared agenthook daemon address is reused,
// so there is no Droid-specific daemon-address option here.
func DefaultOptions() agenthook.Options {
	return agenthook.Options{
		ConfigPath:            config.GlobalConfigPath(),
		TurnThreshold:         DefaultTurnThreshold,
		CommitThreshold:       DefaultCommitThreshold,
		FailedReviewThreshold: DefaultFailedReviewThreshold,
		Instruction:           DefaultInstruction,
	}
}

// ResolveOptions resolves Droid hook options from defaults, the global
// [droid_hook] config section, ROBOREV_DROID_HOOK_* env vars, and CLI
// overrides (changed). It mirrors agenthook.ResolveOptions but reads the
// Droid-specific config section and env vars.
func ResolveOptions(cli agenthook.Options, changed map[string]bool) (agenthook.Options, error) {
	opts := DefaultOptions()
	if changed["config"] {
		opts.ConfigPath = cli.ConfigPath
	}
	if err := applyConfig(&opts); err != nil {
		return agenthook.Options{}, err
	}
	applyEnv(&opts)
	if changed["turn-threshold"] {
		opts.TurnThreshold = cli.TurnThreshold
	}
	if changed["commit-threshold"] {
		opts.CommitThreshold = cli.CommitThreshold
	}
	if changed["failed-review-threshold"] {
		opts.FailedReviewThreshold = cli.FailedReviewThreshold
	}
	if changed["instruction"] {
		opts.Instruction = cli.Instruction
	}
	if changed["roborev-server"] {
		opts.RoborevServerAddr = cli.RoborevServerAddr
	}
	if opts.TurnThreshold < 0 {
		return agenthook.Options{}, fmt.Errorf("turn threshold must be >= 0")
	}
	if opts.CommitThreshold < 0 {
		return agenthook.Options{}, fmt.Errorf("commit threshold must be >= 0")
	}
	if opts.FailedReviewThreshold < 0 {
		return agenthook.Options{}, fmt.Errorf("failed review threshold must be >= 0")
	}
	return opts, nil
}

func applyConfig(opts *agenthook.Options) error {
	cfg, err := config.LoadGlobalFrom(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("load roborev config %s: %w", opts.ConfigPath, err)
	}
	opts.TurnThreshold = cfg.DroidHook.TurnThreshold
	opts.CommitThreshold = cfg.DroidHook.CommitThreshold
	opts.FailedReviewThreshold = cfg.DroidHook.FailedReviewThreshold
	if cfg.DroidHook.Instruction != "" {
		opts.Instruction = cfg.DroidHook.Instruction
	}
	return nil
}

func applyEnv(opts *agenthook.Options) {
	if v, ok := envIntValue(TurnThresholdEnv); ok {
		opts.TurnThreshold = v
	}
	if v, ok := envIntValue(CommitThresholdEnv); ok {
		opts.CommitThreshold = v
	}
	if v, ok := envIntValue(FailedReviewThresholdEnv); ok {
		opts.FailedReviewThreshold = v
	}
	if v := os.Getenv(InstructionEnv); v != "" {
		opts.Instruction = v
	}
	if v := os.Getenv(RoborevServerEnv); v != "" {
		opts.RoborevServerAddr = v
	}
}

func envIntValue(name string) (int, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}
