package agenthook

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

const (
	DefaultTurnThreshold         = 5
	DefaultCommitThreshold       = 0
	DefaultFailedReviewThreshold = 4
	DefaultInstruction           = "Invoke the $roborev-fix skill now."
	DefaultDroidInstruction      = "Run the roborev-fix skill to address the unresolved roborev findings, then continue."

	TurnThresholdEnv         = "ROBOREV_AGENT_HOOK_TURN_THRESHOLD"
	CommitThresholdEnv       = "ROBOREV_AGENT_HOOK_COMMIT_THRESHOLD"
	FailedReviewThresholdEnv = "ROBOREV_AGENT_HOOK_FAILED_REVIEW_THRESHOLD"
	InstructionEnv           = "ROBOREV_AGENT_HOOK_INSTRUCTION"
	RoborevServerEnv         = "ROBOREV_AGENT_HOOK_ROBOREV_ADDR"
	DaemonAddrEnv            = "ROBOREV_AGENT_HOOK_DAEMON_ADDR"

	DroidTurnThresholdEnv         = "ROBOREV_DROID_HOOK_TURN_THRESHOLD"
	DroidCommitThresholdEnv       = "ROBOREV_DROID_HOOK_COMMIT_THRESHOLD"
	DroidFailedReviewThresholdEnv = "ROBOREV_DROID_HOOK_FAILED_REVIEW_THRESHOLD"
	DroidInstructionEnv           = "ROBOREV_DROID_HOOK_INSTRUCTION"
	DroidRoborevServerEnv         = "ROBOREV_DROID_HOOK_ROBOREV_ADDR"
)

type Options struct {
	ConfigPath            string
	TurnThreshold         int
	CommitThreshold       int
	FailedReviewThreshold int
	Instruction           string
	RoborevServerAddr     string
}

func DefaultOptions() Options {
	return Options{
		ConfigPath:            config.GlobalConfigPath(),
		TurnThreshold:         DefaultTurnThreshold,
		CommitThreshold:       DefaultCommitThreshold,
		FailedReviewThreshold: DefaultFailedReviewThreshold,
		Instruction:           DefaultInstruction,
	}
}

func ResolveOptions(cli Options, changed map[string]bool) (Options, error) {
	return ResolveOptionsForAgent("", cli, changed)
}

func ResolveOptionsForAgent(agent string, cli Options, changed map[string]bool) (Options, error) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	switch agent {
	case "", "agent", "codex", "claude":
		return resolveAgentOptions(cli, changed)
	case "droid":
		return resolveDroidOptions(cli, changed)
	default:
		return Options{}, fmt.Errorf("agent must be codex, claude, droid, or empty")
	}
}

func resolveAgentOptions(cli Options, changed map[string]bool) (Options, error) {
	opts := DefaultOptions()
	if changed["config"] {
		opts.ConfigPath = cli.ConfigPath
	}
	if err := applyConfig(&opts); err != nil {
		return Options{}, err
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
		return Options{}, fmt.Errorf("turn threshold must be >= 0")
	}
	if opts.CommitThreshold < 0 {
		return Options{}, fmt.Errorf("commit threshold must be >= 0")
	}
	if opts.FailedReviewThreshold < 0 {
		return Options{}, fmt.Errorf("failed review threshold must be >= 0")
	}
	return opts, nil
}

func resolveDroidOptions(cli Options, changed map[string]bool) (Options, error) {
	opts := DefaultOptions()
	opts.Instruction = DefaultDroidInstruction
	if changed["config"] {
		opts.ConfigPath = cli.ConfigPath
	}
	if err := applyDroidConfig(&opts); err != nil {
		return Options{}, err
	}
	applyDroidEnv(&opts)
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
		return Options{}, fmt.Errorf("turn threshold must be >= 0")
	}
	if opts.CommitThreshold < 0 {
		return Options{}, fmt.Errorf("commit threshold must be >= 0")
	}
	if opts.FailedReviewThreshold < 0 {
		return Options{}, fmt.Errorf("failed review threshold must be >= 0")
	}
	return opts, nil
}

func applyConfig(opts *Options) error {
	cfg, err := config.LoadGlobalFrom(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("load roborev config %s: %w", opts.ConfigPath, err)
	}
	opts.TurnThreshold = cfg.AgentHook.TurnThreshold
	opts.CommitThreshold = cfg.AgentHook.CommitThreshold
	opts.FailedReviewThreshold = cfg.AgentHook.FailedReviewThreshold
	if cfg.AgentHook.Instruction != "" {
		opts.Instruction = cfg.AgentHook.Instruction
	}
	return nil
}

func applyDroidConfig(opts *Options) error {
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

func applyEnv(opts *Options) {
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

func applyDroidEnv(opts *Options) {
	if v, ok := envIntValue(DroidTurnThresholdEnv); ok {
		opts.TurnThreshold = v
	}
	if v, ok := envIntValue(DroidCommitThresholdEnv); ok {
		opts.CommitThreshold = v
	}
	if v, ok := envIntValue(DroidFailedReviewThresholdEnv); ok {
		opts.FailedReviewThreshold = v
	}
	if v := os.Getenv(DroidInstructionEnv); v != "" {
		opts.Instruction = v
	}
	if v := os.Getenv(DroidRoborevServerEnv); v != "" {
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
