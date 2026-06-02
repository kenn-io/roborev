// Auto-design review and routing-classifier configuration.

package config

import (
	"time"

	"go.kenn.io/roborev/internal/review/autotype"
)

// AutoDesignReviewConfig holds settings for the automatic design-review
// decision. Opt-in; when Enabled is false (default), the decision is never
// consulted and behavior matches pre-auto-design-review.
type AutoDesignReviewConfig struct {
	Enabled bool `toml:"enabled" comment:"Enable the automatic design review decider. Off by default."`

	MinDiffLines   int `toml:"min_diff_lines" comment:"Diffs below this line count skip design review automatically."`
	LargeDiffLines int `toml:"large_diff_lines" comment:"Diffs at or above this line count trigger a design review automatically."`
	LargeFileCount int `toml:"large_file_count" comment:"Commits touching at least this many files trigger a design review automatically."`

	TriggerPaths []string `toml:"trigger_paths" comment:"Doublestar globs; any changed file matching triggers a design review."`
	SkipPaths    []string `toml:"skip_paths" comment:"Doublestar globs; a commit whose changed files all match is skipped."`

	TriggerMessagePatterns []string `toml:"trigger_message_patterns" comment:"Regexes over the commit subject; a match triggers a design review."`
	SkipMessagePatterns    []string `toml:"skip_message_patterns" comment:"Regexes over the commit subject; a match skips the design review."`

	ClassifierTimeoutSeconds int `toml:"classifier_timeout_seconds" comment:"Per-classify-job timeout."`
	ClassifierMaxPromptSize  int `toml:"classifier_max_prompt_size" comment:"Cap on classifier prompt size in bytes."`
}

// AutoDesignReviewRepoConfig is the per-repo variant. Identical to the
// global struct except Enabled is *bool, distinguishing "not set" from
// "explicitly false" so a repo can disable a globally-enabled default.
type AutoDesignReviewRepoConfig struct {
	Enabled *bool `toml:"enabled" comment:"Override the global auto design review setting for this repo. Omit to inherit."`

	MinDiffLines   int `toml:"min_diff_lines"`
	LargeDiffLines int `toml:"large_diff_lines"`
	LargeFileCount int `toml:"large_file_count"`

	TriggerPaths []string `toml:"trigger_paths"`
	SkipPaths    []string `toml:"skip_paths"`

	TriggerMessagePatterns []string `toml:"trigger_message_patterns"`
	SkipMessagePatterns    []string `toml:"skip_message_patterns"`

	ClassifierTimeoutSeconds int `toml:"classifier_timeout_seconds"`
	ClassifierMaxPromptSize  int `toml:"classifier_max_prompt_size"`
}

// AutoDesignHeuristics is the resolved heuristic configuration returned by
// ResolveAutoDesignHeuristics. Matches internal/review/autotype.Heuristics
// field-for-field so callers can construct one from the other; keeping the
// type here avoids a circular import.
type AutoDesignHeuristics struct {
	MinDiffLines   int
	LargeDiffLines int
	LargeFileCount int

	TriggerPaths []string
	SkipPaths    []string

	TriggerMessagePatterns []string
	SkipMessagePatterns    []string
}

// DefaultAutoDesignHeuristics returns the embedded fallback values. Keep in
// sync with autotype.DefaultHeuristics.
func DefaultAutoDesignHeuristics() AutoDesignHeuristics {
	return AutoDesignHeuristics{
		MinDiffLines:   10,
		LargeDiffLines: 500,
		LargeFileCount: 10,
		TriggerPaths: []string{
			"**/migrations/**",
			"**/schema/**",
			"**/*.sql",
			"docs/superpowers/specs/**",
			"docs/design/**",
			"docs/plans/**",
			"**/*-design.md",
			"**/*-plan.md",
		},
		SkipPaths: []string{
			"**/*.md",
			"**/*_test.go",
			"**/*.spec.*",
			"**/testdata/**",
		},
		TriggerMessagePatterns: []string{
			`\b(refactor|redesign|rewrite|architect|breaking)\b`,
		},
		SkipMessagePatterns: []string{
			`^(docs|test|style|chore)(\(.+\))?:`,
		},
	}
}

// ResolveAutoDesignEnabled returns true if auto design review is enabled for
// the given repo path. Tri-state: a per-repo explicit `enabled = false`
// overrides a globally-enabled default, and a per-repo `enabled = true`
// overrides a globally-disabled default. An unset per-repo field falls
// through to the global value. Delegates to AutoDesignEnabledFromConfig after
// loading the repo config.
func ResolveAutoDesignEnabled(repoPath string, globalCfg *Config) bool {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return AutoDesignEnabledFromConfig(repoCfg, globalCfg)
}

// AutoDesignEnabledFromConfig is the config-taking core of
// ResolveAutoDesignEnabled: it resolves the tri-state enabled flag entirely
// from the passed repoCfg and globalCfg, never reading the working tree. Use
// this in contexts (e.g. CI) that must resolve from a config loaded off the
// default branch (F12). A nil repoCfg falls through to the global value.
func AutoDesignEnabledFromConfig(repoCfg *RepoConfig, globalCfg *Config) bool {
	if repoCfg != nil && repoCfg.AutoDesignReview.Enabled != nil {
		return *repoCfg.AutoDesignReview.Enabled
	}
	if globalCfg != nil && globalCfg.AutoDesignReview.Enabled {
		return true
	}
	return false
}

// ResolveAutoDesignHeuristics merges defaults, global, and per-repo config.
// List fields replace wholesale; scalar fields fall through when zero.
// Delegates to AutoDesignHeuristicsFromConfig after loading the repo config.
func ResolveAutoDesignHeuristics(repoPath string, globalCfg *Config) AutoDesignHeuristics {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return AutoDesignHeuristicsFromConfig(repoCfg, globalCfg)
}

// AutoDesignHeuristicsFromConfig is the config-taking core of
// ResolveAutoDesignHeuristics: it merges defaults, the global overlay, then the
// per-repo overlay entirely from the passed configs, never reading the working
// tree (F12). A nil globalCfg or repoCfg skips that overlay layer. List fields
// replace wholesale; scalar fields fall through when zero.
func AutoDesignHeuristicsFromConfig(repoCfg *RepoConfig, globalCfg *Config) AutoDesignHeuristics {
	h := DefaultAutoDesignHeuristics()
	if globalCfg != nil {
		h = overlayAutoDesignGlobal(h, globalCfg.AutoDesignReview)
	}
	if repoCfg != nil {
		h = overlayAutoDesignRepo(h, repoCfg.AutoDesignReview)
	}
	return h
}

// ResolveGlobalAutoDesignHeuristics merges defaults with the global overlay
// only. Used at daemon startup where no specific repo context applies and
// per-repo validation would require an open DB to enumerate repos. Equivalent
// to AutoDesignHeuristicsFromConfig with a nil repo config.
func ResolveGlobalAutoDesignHeuristics(globalCfg *Config) AutoDesignHeuristics {
	return AutoDesignHeuristicsFromConfig(nil, globalCfg)
}

// DesignAgentFromConfig resolves the design-review agent and model PREFERENCE
// entirely from the passed repoCfg and globalCfg, never reading the working
// tree (F12). It does NOT perform any machine-local availability check; callers
// (e.g. the auto-design dispatch site) select an installed agent separately.
//
// The model follows the same nuance as panel members: when a design-workflow
// agent is explicitly configured, the omitted model inherits only a
// workflow-specific design model (never a generic default_model/repo model
// paired with a different default agent); otherwise the generic workflow model
// resolution applies.
func DesignAgentFromConfig(repoCfg *RepoConfig, globalCfg *Config) (agent string, model string) {
	agent = ResolveAgentForWorkflowFromConfig("", repoCfg, globalCfg, ReviewTypeDesign, "")
	if designAgentPinned(repoCfg, globalCfg) {
		model = ResolveWorkflowModelFromConfig(repoCfg, globalCfg, ReviewTypeDesign, "")
	} else {
		model = ResolveModelForWorkflowFromConfig("", repoCfg, globalCfg, ReviewTypeDesign, "")
	}
	return agent, model
}

// designAgentPinned reports whether a design-workflow agent is explicitly
// configured (the workflow-generic design_agent field) on either the repo or
// global config, as opposed to falling through to a generic agent/default_agent.
// This mirrors the panel member rule that keys off an explicitly pinned subagent
// agent: a pinned design agent must not inherit a generic model paired with a
// different default agent. Resolution happens at level "", so only the
// workflow-generic design_agent field is consulted.
func designAgentPinned(repoCfg *RepoConfig, globalCfg *Config) bool {
	if repoCfg != nil && repoWorkflowField(repoCfg, ReviewTypeDesign, "", true) != "" {
		return true
	}
	if globalCfg != nil && globalWorkflowField(globalCfg, ReviewTypeDesign, "", true) != "" {
		return true
	}
	return false
}

// Validate compiles each regex and checks each glob, surfacing invalid
// trigger/skip patterns so config typos fail loudly instead of silently
// suppressing every auto-design dispatch at runtime. Delegates to
// autotype.Heuristics.Validate so the pattern checks live in one place.
func (h AutoDesignHeuristics) Validate() error {
	return autotype.Heuristics{
		MinDiffLines:           h.MinDiffLines,
		LargeDiffLines:         h.LargeDiffLines,
		LargeFileCount:         h.LargeFileCount,
		TriggerPaths:           h.TriggerPaths,
		SkipPaths:              h.SkipPaths,
		TriggerMessagePatterns: h.TriggerMessagePatterns,
		SkipMessagePatterns:    h.SkipMessagePatterns,
	}.Validate()
}

func overlayAutoDesignGlobal(base AutoDesignHeuristics, over AutoDesignReviewConfig) AutoDesignHeuristics {
	if over.MinDiffLines > 0 {
		base.MinDiffLines = over.MinDiffLines
	}
	if over.LargeDiffLines > 0 {
		base.LargeDiffLines = over.LargeDiffLines
	}
	if over.LargeFileCount > 0 {
		base.LargeFileCount = over.LargeFileCount
	}
	// nil vs non-nil empty is distinguishable: unset keeps the base
	// defaults; an explicit empty TOML list (e.g. trigger_paths = [])
	// clears them. Using len(...) > 0 would silently conflate the two
	// and prevent users from disabling a heuristic family.
	if over.TriggerPaths != nil {
		base.TriggerPaths = over.TriggerPaths
	}
	if over.SkipPaths != nil {
		base.SkipPaths = over.SkipPaths
	}
	if over.TriggerMessagePatterns != nil {
		base.TriggerMessagePatterns = over.TriggerMessagePatterns
	}
	if over.SkipMessagePatterns != nil {
		base.SkipMessagePatterns = over.SkipMessagePatterns
	}
	return base
}

func overlayAutoDesignRepo(base AutoDesignHeuristics, over AutoDesignReviewRepoConfig) AutoDesignHeuristics {
	if over.MinDiffLines > 0 {
		base.MinDiffLines = over.MinDiffLines
	}
	if over.LargeDiffLines > 0 {
		base.LargeDiffLines = over.LargeDiffLines
	}
	if over.LargeFileCount > 0 {
		base.LargeFileCount = over.LargeFileCount
	}
	// nil vs non-nil empty is distinguishable: unset keeps the base
	// defaults; an explicit empty TOML list (e.g. trigger_paths = [])
	// clears them. Using len(...) > 0 would silently conflate the two
	// and prevent users from disabling a heuristic family.
	if over.TriggerPaths != nil {
		base.TriggerPaths = over.TriggerPaths
	}
	if over.SkipPaths != nil {
		base.SkipPaths = over.SkipPaths
	}
	if over.TriggerMessagePatterns != nil {
		base.TriggerMessagePatterns = over.TriggerMessagePatterns
	}
	if over.SkipMessagePatterns != nil {
		base.SkipMessagePatterns = over.SkipMessagePatterns
	}
	return base
}

// ClassifyAgentValidator is an injection point the agent package fills in
// via RegisterClassifyAgentValidator. It returns an error when the named
// agent does not implement structured-output (SchemaAgent) capability.
//
// If nil, validation is skipped (useful in tests and at the lowest layer).
var classifyAgentValidator func(name string) error

// RegisterClassifyAgentValidator is called by the agent package during init
// to provide SchemaAgent support verification.
func RegisterClassifyAgentValidator(fn func(name string) error) {
	classifyAgentValidator = fn
}

const (
	DefaultClassifyAgent     = "claude-code"
	DefaultClassifyReasoning = "fast"
)

// ResolveClassifyAgent returns the agent name to use for classification.
// Priority: CLI flag > per-repo classify_agent > global classify_agent > default.
// Validates via the registered validator (SchemaAgent capability check).
func ResolveClassifyAgent(cliAgent, repoPath string, globalCfg *Config) (string, error) {
	name := cliAgent
	if name == "" {
		if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ClassifyAgent != "" {
			name = repoCfg.ClassifyAgent
		}
	}
	if name == "" && globalCfg != nil && globalCfg.ClassifyAgent != "" {
		name = globalCfg.ClassifyAgent
	}
	if name == "" {
		name = DefaultClassifyAgent
	}
	if classifyAgentValidator != nil {
		if err := classifyAgentValidator(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// ResolveClassifyModel returns the model string for the classifier. Priority
// same as ResolveClassifyAgent. Empty is a valid value (agent uses its default).
func ResolveClassifyModel(cliModel, repoPath string, globalCfg *Config) string {
	if cliModel != "" {
		return cliModel
	}
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ClassifyModel != "" {
		return repoCfg.ClassifyModel
	}
	if globalCfg != nil && globalCfg.ClassifyModel != "" {
		return globalCfg.ClassifyModel
	}
	return ""
}

// ResolveClassifyReasoning returns the reasoning level for the classifier.
// Defaults to "fast" since the classifier is a routing decision, not a review.
func ResolveClassifyReasoning(cliReasoning, repoPath string, globalCfg *Config) string {
	if cliReasoning != "" {
		return cliReasoning
	}
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ClassifyReasoning != "" {
		return repoCfg.ClassifyReasoning
	}
	if globalCfg != nil && globalCfg.ClassifyReasoning != "" {
		return globalCfg.ClassifyReasoning
	}
	return DefaultClassifyReasoning
}

// ResolveBackupClassifyAgent returns the backup classify agent (no
// default). Like ResolveClassifyAgent, it validates the configured name
// via the registered SchemaAgent-capability hook — a backup that can't
// actually run the classify workflow would silently cause skips instead
// of a real failover. An unconfigured backup (empty result) is not an
// error; callers treat empty as "no backup".
func ResolveBackupClassifyAgent(repoPath string, globalCfg *Config) (string, error) {
	var name string
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ClassifyBackupAgent != "" {
		name = repoCfg.ClassifyBackupAgent
	}
	if name == "" && globalCfg != nil && globalCfg.ClassifyBackupAgent != "" {
		name = globalCfg.ClassifyBackupAgent
	}
	if name == "" {
		return "", nil
	}
	if classifyAgentValidator != nil {
		if err := classifyAgentValidator(name); err != nil {
			return "", err
		}
	}
	return name, nil
}

// ResolveBackupClassifyModel returns the backup classify model (no default).
func ResolveBackupClassifyModel(repoPath string, globalCfg *Config) string {
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ClassifyBackupModel != "" {
		return repoCfg.ClassifyBackupModel
	}
	if globalCfg != nil && globalCfg.ClassifyBackupModel != "" {
		return globalCfg.ClassifyBackupModel
	}
	return ""
}

const (
	defaultClassifierTimeoutSeconds = 60
	defaultClassifierMaxPromptSize  = 20 * 1024
)

// ResolveClassifierTimeout returns the per-classify-job timeout.
func ResolveClassifierTimeout(repoPath string, globalCfg *Config) time.Duration {
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.AutoDesignReview.ClassifierTimeoutSeconds > 0 {
		return time.Duration(repoCfg.AutoDesignReview.ClassifierTimeoutSeconds) * time.Second
	}
	if globalCfg != nil && globalCfg.AutoDesignReview.ClassifierTimeoutSeconds > 0 {
		return time.Duration(globalCfg.AutoDesignReview.ClassifierTimeoutSeconds) * time.Second
	}
	return defaultClassifierTimeoutSeconds * time.Second
}

// ResolveClassifierMaxPromptSize returns the cap on classify-prompt bytes.
func ResolveClassifierMaxPromptSize(repoPath string, globalCfg *Config) int {
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.AutoDesignReview.ClassifierMaxPromptSize > 0 {
		return repoCfg.AutoDesignReview.ClassifierMaxPromptSize
	}
	if globalCfg != nil && globalCfg.AutoDesignReview.ClassifierMaxPromptSize > 0 {
		return globalCfg.AutoDesignReview.ClassifierMaxPromptSize
	}
	return defaultClassifierMaxPromptSize
}

// ResolveShowClassifyJobs returns whether the TUI queue should display
// auto-design-review classifier rows (job_type=classify) and skipped
// design rows (status=skipped). Off by default to reduce queue noise
// when the auto-design router is enabled. Per-repo config overrides
// global.
func ResolveShowClassifyJobs(repoPath string, globalCfg *Config) bool {
	var repoVal *bool
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		repoVal = repoCfg.ShowClassifyJobs
	}
	var globalVal bool
	if globalCfg != nil {
		globalVal = globalCfg.ShowClassifyJobs
	}
	return resolveBool(globalVal, repoVal)
}
