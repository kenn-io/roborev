package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	tomlv2 "github.com/pelletier/go-toml/v2"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/review/autotype"
)

// ConfigParseError is returned when .roborev.toml exists but
// contains invalid TOML. Callers can check with errors.As.
type ConfigParseError struct {
	Ref string
	Err error
}

func (e *ConfigParseError) Error() string {
	return fmt.Sprintf("parse .roborev.toml at %s: %v", e.Ref, e.Err)
}

func (e *ConfigParseError) Unwrap() error { return e.Err }

// IsConfigParseError reports whether err (or any error in its chain)
// is a ConfigParseError.
func IsConfigParseError(err error) bool {
	var pe *ConfigParseError
	if errors.As(err, &pe) {
		return true
	}
	var syntaxErr toml.ParseError
	return errors.As(err, &syntaxErr)
}

// HookConfig defines a hook that runs on review events
type HookConfig struct {
	Event   string `toml:"event"`                // "review.failed", "review.completed", "review.*"
	Command string `toml:"command"`              // shell command with {var} templates
	Type    string `toml:"type"`                 // "beads" or "webhook"; empty or "command" runs Command
	URL     string `toml:"url" sensitive:"true"` // webhook destination URL when Type is "webhook"
}

type AdvancedConfig struct {
	TasksEnabled bool `toml:"tasks_enabled" comment:"Enable the advanced Tasks workflow in the TUI."` // Enables advanced TUI tasks workflow
}

type CodexConfig struct {
	DisableReviewSkills    bool `toml:"disable_review_skills" comment:"Disable Codex skill instructions for review jobs."`
	IgnoreReviewUserConfig bool `toml:"ignore_review_user_config" comment:"Pass --ignore-user-config to Codex for review jobs."`
}

type AgentConfig struct {
	Codex CodexConfig `toml:"codex"`
}

// Config holds the daemon configuration
type Config struct {
	ServerAddr                 string `toml:"server_addr"`
	MaxWorkers                 int    `toml:"max_workers"`
	ReviewContextCount         int    `toml:"review_context_count"`
	ReuseReviewSessionLookback int    `toml:"reuse_review_session_lookback"` // 0 means no candidate cap
	DefaultAgent               string `toml:"default_agent" comment:"Default agent when no workflow-specific agent is set."`
	DefaultModel               string `toml:"default_model"` // Default model for agents (format varies by agent)
	DefaultBackupAgent         string `toml:"default_backup_agent"`
	DefaultBackupModel         string `toml:"default_backup_model"`
	JobTimeoutMinutes          int    `toml:"job_timeout_minutes"`
	ReviewReasoning            string `toml:"review_reasoning" comment:"Default reasoning level for reviews: fast, standard, medium, thorough, or maximum."`
	RefineReasoning            string `toml:"refine_reasoning" comment:"Default reasoning level for refine: fast, standard, medium, thorough, or maximum."`
	FixReasoning               string `toml:"fix_reasoning" comment:"Default reasoning level for fix: fast, standard, medium, thorough, or maximum."`

	// Workflow-specific agent/model configuration
	ReviewAgent           string `toml:"review_agent"`
	ReviewAgentFast       string `toml:"review_agent_fast"`
	ReviewAgentStandard   string `toml:"review_agent_standard"`
	ReviewAgentMedium     string `toml:"review_agent_medium"`
	ReviewAgentThorough   string `toml:"review_agent_thorough"`
	ReviewAgentMaximum    string `toml:"review_agent_maximum"`
	RefineAgent           string `toml:"refine_agent"`
	RefineAgentFast       string `toml:"refine_agent_fast"`
	RefineAgentStandard   string `toml:"refine_agent_standard"`
	RefineAgentMedium     string `toml:"refine_agent_medium"`
	RefineAgentThorough   string `toml:"refine_agent_thorough"`
	RefineAgentMaximum    string `toml:"refine_agent_maximum"`
	ReviewModel           string `toml:"review_model"`
	ReviewModelFast       string `toml:"review_model_fast"`
	ReviewModelStandard   string `toml:"review_model_standard"`
	ReviewModelMedium     string `toml:"review_model_medium"`
	ReviewModelThorough   string `toml:"review_model_thorough"`
	ReviewModelMaximum    string `toml:"review_model_maximum"`
	RefineModel           string `toml:"refine_model"`
	RefineModelFast       string `toml:"refine_model_fast"`
	RefineModelStandard   string `toml:"refine_model_standard"`
	RefineModelMedium     string `toml:"refine_model_medium"`
	RefineModelThorough   string `toml:"refine_model_thorough"`
	RefineModelMaximum    string `toml:"refine_model_maximum"`
	FixAgent              string `toml:"fix_agent"`
	FixAgentFast          string `toml:"fix_agent_fast"`
	FixAgentStandard      string `toml:"fix_agent_standard"`
	FixAgentMedium        string `toml:"fix_agent_medium"`
	FixAgentThorough      string `toml:"fix_agent_thorough"`
	FixAgentMaximum       string `toml:"fix_agent_maximum"`
	FixModel              string `toml:"fix_model"`
	FixModelFast          string `toml:"fix_model_fast"`
	FixModelStandard      string `toml:"fix_model_standard"`
	FixModelMedium        string `toml:"fix_model_medium"`
	FixModelThorough      string `toml:"fix_model_thorough"`
	FixModelMaximum       string `toml:"fix_model_maximum"`
	SecurityAgent         string `toml:"security_agent"`
	SecurityAgentFast     string `toml:"security_agent_fast"`
	SecurityAgentStandard string `toml:"security_agent_standard"`
	SecurityAgentMedium   string `toml:"security_agent_medium"`
	SecurityAgentThorough string `toml:"security_agent_thorough"`
	SecurityAgentMaximum  string `toml:"security_agent_maximum"`
	SecurityModel         string `toml:"security_model"`
	SecurityModelFast     string `toml:"security_model_fast"`
	SecurityModelStandard string `toml:"security_model_standard"`
	SecurityModelMedium   string `toml:"security_model_medium"`
	SecurityModelThorough string `toml:"security_model_thorough"`
	SecurityModelMaximum  string `toml:"security_model_maximum"`
	DesignAgent           string `toml:"design_agent"`
	DesignAgentFast       string `toml:"design_agent_fast"`
	DesignAgentStandard   string `toml:"design_agent_standard"`
	DesignAgentMedium     string `toml:"design_agent_medium"`
	DesignAgentThorough   string `toml:"design_agent_thorough"`
	DesignAgentMaximum    string `toml:"design_agent_maximum"`
	DesignModel           string `toml:"design_model"`
	DesignModelFast       string `toml:"design_model_fast"`
	DesignModelStandard   string `toml:"design_model_standard"`
	DesignModelMedium     string `toml:"design_model_medium"`
	DesignModelThorough   string `toml:"design_model_thorough"`
	DesignModelMaximum    string `toml:"design_model_maximum"`

	// Classify workflow (routing classifier for auto design review)
	ClassifyAgent       string `toml:"classify_agent" comment:"Agent for the design-review routing classifier. Must implement SchemaAgent capability."`
	ClassifyModel       string `toml:"classify_model" comment:"Model for the classifier agent. Empty = agent default."`
	ClassifyReasoning   string `toml:"classify_reasoning" comment:"Reasoning level for the classifier: fast, standard, medium, thorough, or maximum."`
	ClassifyBackupAgent string `toml:"classify_backup_agent" comment:"Fallback classifier agent on quota exhaustion / failure."`
	ClassifyBackupModel string `toml:"classify_backup_model" comment:"Fallback classifier model."`

	// Backup agents for failover
	ReviewBackupAgent   string `toml:"review_backup_agent"`
	RefineBackupAgent   string `toml:"refine_backup_agent"`
	FixBackupAgent      string `toml:"fix_backup_agent"`
	SecurityBackupAgent string `toml:"security_backup_agent"`
	DesignBackupAgent   string `toml:"design_backup_agent"`

	// Backup models for failover (used when failing over to backup agent)
	ReviewBackupModel   string `toml:"review_backup_model"`
	RefineBackupModel   string `toml:"refine_backup_model"`
	FixBackupModel      string `toml:"fix_backup_model"`
	SecurityBackupModel string `toml:"security_backup_model"`
	DesignBackupModel   string `toml:"design_backup_model"`

	// Minimum severity thresholds (global defaults)
	ReviewMinSeverity string `toml:"review_min_severity" comment:"Minimum severity for reviews: critical, high, medium, or low. Empty disables filtering."`
	RefineMinSeverity string `toml:"refine_min_severity" comment:"Minimum severity for refine: critical, high, medium, or low. Empty disables filtering."`
	FixMinSeverity    string `toml:"fix_min_severity" comment:"Minimum severity for fix: critical, high, medium, or low. Empty disables filtering."`

	AllowUnsafeAgents   *bool `toml:"allow_unsafe_agents"`   // nil = not set, allows commands to choose their own default
	DisableCodexSandbox bool  `toml:"disable_codex_sandbox"` // use --full-auto instead of --sandbox read-only (for systems where bwrap is broken)
	ReuseReviewSession  *bool `toml:"reuse_review_session"`  // nil = not set; when true, reuse prior branch review sessions when possible

	// Agent commands
	CodexCmd      string `toml:"codex_cmd"`
	ClaudeCodeCmd string `toml:"claude_code_cmd"`
	CursorCmd     string `toml:"cursor_cmd"`
	PiCmd         string `toml:"pi_cmd"`
	OpenCodeCmd   string `toml:"opencode_cmd"`

	// API keys (optional - agents use subscription auth by default)
	AnthropicAPIKey string `toml:"anthropic_api_key" sensitive:"true"`

	// Hooks configuration
	Hooks []HookConfig `toml:"hooks"`

	// Sync configuration for PostgreSQL
	Sync SyncConfig `toml:"sync"`

	// CI poller configuration
	CI CIConfig `toml:"ci"`

	// Agent-specific behavior
	Agent AgentConfig `toml:"agent"`

	// Auto design review configuration (opt-in)
	AutoDesignReview AutoDesignReviewConfig `toml:"auto_design_review"`

	// Diff exclusion patterns (filenames or glob patterns to exclude from review diffs)
	ExcludePatterns []string `toml:"exclude_patterns" comment:"Filenames or glob patterns to exclude from review diffs globally."`

	// Analysis settings
	DefaultMaxPromptSize int `toml:"default_max_prompt_size"` // Max prompt size in bytes before falling back to paths (default: 200KB)

	// Behavior
	AutoClosePassingReviews bool `toml:"auto_close_passing_reviews" comment:"Automatically close reviews that pass with no findings."`

	// UI preferences
	HideClosedByDefault    bool     `toml:"hide_closed_by_default" comment:"Hide closed reviews by default in the TUI queue."`
	HideAddressedByDefault bool     `toml:"hide_addressed_by_default"` // deprecated: use hide_closed_by_default
	AutoFilterRepo         bool     `toml:"auto_filter_repo" comment:"Automatically filter the TUI queue to the current repo."`
	AutoFilterBranch       bool     `toml:"auto_filter_branch" comment:"Automatically filter the TUI queue to the current branch."`
	ShowClassifyJobs       bool     `toml:"show_classify_jobs" comment:"Show auto-design-review classifier rows (and skipped design rows) in the TUI queue. Off by default to reduce noise."`
	MouseEnabled           bool     `toml:"mouse_enabled" comment:"Enable mouse support in the TUI."`          // Enable mouse capture and mouse-driven TUI interactions
	TabWidth               int      `toml:"tab_width"`                                                         // Tab expansion width for TUI rendering (default: 2)
	HiddenColumns          []string `toml:"hidden_columns" comment:"Queue columns to hide in the TUI."`        // Column names to hide in queue table (e.g. ["branch", "agent"])
	ColumnBorders          bool     `toml:"column_borders" comment:"Show column borders in the TUI queue."`    // Show ▕ separators between columns
	ColumnOrder            []string `toml:"column_order" comment:"Custom queue column order in the TUI."`      // Custom queue column display order
	TaskColumnOrder        []string `toml:"task_column_order" comment:"Custom Tasks column order in the TUI."` // Custom task column display order
	ColumnConfigVersion    int      `toml:"column_config_version"`                                             // Tracks column migration version to avoid re-running one-shot migrations

	// Advanced feature flags
	Advanced AdvancedConfig `toml:"advanced"`

	// ACP (Agent Client Protocol) configuration
	ACP *ACPAgentConfig `toml:"acp"`
}

// ACPAgentConfig holds configuration for a single ACP agent
type ACPAgentConfig struct {
	Name            string   `toml:"name"`              // Agent name (required)
	Command         string   `toml:"command"`           // ACP agent command (required)
	Args            []string `toml:"args"`              // Additional arguments for the agent
	ReadOnlyMode    string   `toml:"read_only_mode"`    // Read-only mode. Valid values depend on the underlying agent, e.g. "plan"
	AutoApproveMode string   `toml:"auto_approve_mode"` // Auto-approve mode. Valid values depend on the underlying agent, e.g. "auto-approve"
	Mode            string   `toml:"mode"`              // Default agent mode. Use read_only_mode for review flows unless explicitly opting in.
	// DisableModeNegotiation skips ACP SetSessionMode while keeping
	// authorization behavior based on agentic/read-only mode selection.
	DisableModeNegotiation bool   `toml:"disable_mode_negotiation"`
	Model                  string `toml:"model"`   // Default model to use
	Timeout                int    `toml:"timeout"` // Command timeout in seconds (default: 600)
}

// GitHubAppConfig holds GitHub App authentication settings.
// Extracted from CIConfig for cohesion; embedded so TOML keys remain flat under [ci].
type GitHubAppConfig struct {
	GitHubAppID             int64  `toml:"github_app_id"`
	GitHubAppPrivateKey     string `toml:"github_app_private_key" sensitive:"true"` // PEM file path or inline; supports ${ENV_VAR}
	GitHubAppInstallationID int64  `toml:"github_app_installation_id"`

	// Multi-installation: map of owner → installation_id
	GitHubAppInstallations map[string]int64 `toml:"github_app_installations"`
}

// GitHubAppConfigured returns true if GitHub App authentication can be used.
// Requires app ID, private key, and at least one installation ID (singular or map).
func (c *GitHubAppConfig) GitHubAppConfigured() bool {
	return c.GitHubAppID != 0 && c.GitHubAppPrivateKey != "" &&
		(c.GitHubAppInstallationID != 0 || len(c.GitHubAppInstallations) > 0)
}

// InstallationIDForOwner returns the installation ID for a GitHub owner.
// Checks the normalized installations map first (skipping non-positive values),
// then falls back to the singular field. Owner comparison is case-insensitive.
func (c *GitHubAppConfig) InstallationIDForOwner(owner string) int64 {
	if id, ok := c.GitHubAppInstallations[strings.ToLower(owner)]; ok && id > 0 {
		return id
	}
	return c.GitHubAppInstallationID
}

// NormalizeInstallations lowercases all keys in GitHubAppInstallations
// so lookups are case-insensitive via direct map access.
// Returns an error if two keys collide after lowercasing (e.g., "wesm" and "Wesm").
func (c *GitHubAppConfig) NormalizeInstallations() error {
	if len(c.GitHubAppInstallations) == 0 {
		return nil
	}
	normalized := make(map[string]int64, len(c.GitHubAppInstallations))
	for k, v := range c.GitHubAppInstallations {
		lower := strings.ToLower(k)
		if _, exists := normalized[lower]; exists {
			return fmt.Errorf("case-colliding github_app_installations keys for %q", lower)
		}
		normalized[lower] = v
	}
	c.GitHubAppInstallations = normalized
	return nil
}

// GitHubAppPrivateKeyResolved expands env vars in the private key value,
// reads the file if it's a path, and returns the PEM content.
func (c *GitHubAppConfig) GitHubAppPrivateKeyResolved() (string, error) {
	val := os.ExpandEnv(c.GitHubAppPrivateKey)
	if val == "" {
		return "", fmt.Errorf("github_app_private_key is empty after expansion")
	}

	// If it looks like PEM content, return directly
	// TrimSpace handles leading whitespace/newlines in inline PEM content
	trimmed := strings.TrimSpace(val)
	if strings.HasPrefix(trimmed, "-----BEGIN") {
		return trimmed, nil
	}

	// Expand leading ~ to home directory
	if strings.HasPrefix(val, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home for github_app_private_key: %w", err)
		}
		val = home + val[1:]
	}

	// Otherwise treat as file path
	data, err := os.ReadFile(val)
	if err != nil {
		return "", fmt.Errorf("read private key file %s: %w", val, err)
	}
	return string(data), nil
}

// AgentReviewType pairs an agent name with a review type for the review matrix.
type AgentReviewType struct {
	Agent      string
	ReviewType string
}

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
// through to the global value.
func ResolveAutoDesignEnabled(repoPath string, globalCfg *Config) bool {
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.AutoDesignReview.Enabled != nil {
		return *repoCfg.AutoDesignReview.Enabled
	}
	if globalCfg != nil && globalCfg.AutoDesignReview.Enabled {
		return true
	}
	return false
}

// ResolveAutoDesignHeuristics merges defaults, global, and per-repo config.
// List fields replace wholesale; scalar fields fall through when zero.
func ResolveAutoDesignHeuristics(repoPath string, globalCfg *Config) AutoDesignHeuristics {
	h := DefaultAutoDesignHeuristics()
	if globalCfg != nil {
		h = overlayAutoDesignGlobal(h, globalCfg.AutoDesignReview)
	}
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		h = overlayAutoDesignRepo(h, repoCfg.AutoDesignReview)
	}
	return h
}

// ResolveGlobalAutoDesignHeuristics merges defaults with the global overlay
// only. Used at daemon startup where no specific repo context applies and
// per-repo validation would require an open DB to enumerate repos.
func ResolveGlobalAutoDesignHeuristics(globalCfg *Config) AutoDesignHeuristics {
	h := DefaultAutoDesignHeuristics()
	if globalCfg != nil {
		h = overlayAutoDesignGlobal(h, globalCfg.AutoDesignReview)
	}
	return h
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

const DefaultClassifyAgent = "claude-code"
const DefaultClassifyReasoning = "fast"

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

// CIConfig holds configuration for the CI poller that watches GitHub PRs
type CIConfig struct {
	// Enabled enables the CI poller
	Enabled bool `toml:"enabled"`

	// PollInterval is how often to poll for PRs (e.g., "5m", "10m"). Default: 5m
	PollInterval string `toml:"poll_interval"`

	// Repos is the list of GitHub repos to poll in "owner/repo" format.
	// Supports glob patterns (e.g., "myorg/*", "myorg/api-*") using path.Match syntax.
	// The owner part must be literal — no wildcards before the "/".
	Repos []string `toml:"repos"`

	// ExcludeRepos is a list of glob patterns to exclude from the resolved repo list.
	// Applies to both exact entries and wildcard-expanded entries.
	ExcludeRepos []string `toml:"exclude_repos"`

	// MaxRepos is a safety cap on the total number of expanded repos. Default: 100.
	MaxRepos int `toml:"max_repos"`

	// ReviewTypes is the list of review types to run for each PR (e.g., ["security", "default"]).
	// Defaults to ["security"] if empty.
	ReviewTypes []string `toml:"review_types"`

	// Agents is the list of agents to run for each PR (e.g., ["codex", "gemini"]).
	// Defaults to auto-detection if empty.
	Agents []string `toml:"agents"`

	// Reviews maps agent names to review type lists. When set, replaces
	// the ReviewTypes x Agents cross-product with a granular matrix.
	// Example: {"codex": ["security", "review"], "gemini": ["review"]}
	Reviews map[string][]string `toml:"reviews"`

	// ThrottleInterval is the minimum time between reviews of the same PR.
	// If a PR was reviewed within this interval, new pushes are deferred.
	// Default: "1h". Set to "0" to disable throttling.
	ThrottleInterval string `toml:"throttle_interval"`

	// ThrottleBypassUsers is a list of GitHub usernames whose PRs
	// bypass the throttle interval and are always reviewed immediately.
	ThrottleBypassUsers []string `toml:"throttle_bypass_users"`

	// Model overrides the model for CI reviews (empty = use workflow resolution)
	Model string `toml:"model"`

	// SynthesisAgent is the agent used to synthesize multiple review outputs into one comment.
	// Defaults to the first available agent.
	SynthesisAgent string `toml:"synthesis_agent"`

	// SynthesisBackupAgent is tried when the primary synthesis
	// agent fails. Empty means no backup — failures fall through
	// to raw formatting.
	SynthesisBackupAgent string `toml:"synthesis_backup_agent"`

	// SynthesisModel overrides the model used for synthesis.
	SynthesisModel string `toml:"synthesis_model"`

	// MinSeverity filters out findings below this severity level during synthesis.
	// Valid values: critical, high, medium, low. Empty means no filter (include all).
	MinSeverity string `toml:"min_severity"`

	// UpsertComments enables updating existing PR comments instead of
	// creating new ones. When true, roborev searches for its marker
	// comment and patches it. Default: false (create a new comment each run).
	UpsertComments bool `toml:"upsert_comments"`

	// BatchTimeout is how long to wait for all batch jobs to complete before
	// posting results with available reviews. Jobs still running after this
	// timeout are canceled. Default: "3m". Set to "0" to disable.
	BatchTimeout string `toml:"batch_timeout"`

	// GitHub App authentication (optional — comments appear as bot instead of personal account)
	GitHubAppConfig
}

// ResolvedReviewTypes returns the list of review types to use.
// Defaults to ["security"] if empty.
func (c *CIConfig) ResolvedReviewTypes() []string {
	if len(c.ReviewTypes) > 0 {
		return c.ReviewTypes
	}
	return []string{ReviewTypeSecurity}
}

// ResolvedAgents returns the list of agents to use.
// Defaults to [""] (empty = auto-detect) if empty.
func (c *CIConfig) ResolvedAgents() []string {
	if len(c.Agents) > 0 {
		return c.Agents
	}
	return []string{""}
}

// ResolvedReviewMatrix returns (agent, reviewType) pairs.
// If Reviews is set, uses it directly. Otherwise falls back to
// the cross-product of ResolvedAgents() x ResolvedReviewTypes().
func (c *CIConfig) ResolvedReviewMatrix() []AgentReviewType {
	if len(c.Reviews) > 0 {
		return reviewsMapToMatrix(c.Reviews)
	}
	agents := c.ResolvedAgents()
	reviewTypes := c.ResolvedReviewTypes()
	matrix := make(
		[]AgentReviewType, 0, len(agents)*len(reviewTypes),
	)
	for _, rt := range reviewTypes {
		for _, ag := range agents {
			matrix = append(matrix, AgentReviewType{
				Agent:      ag,
				ReviewType: rt,
			})
		}
	}
	return matrix
}

// ResolvedReviewMatrixForRepo returns the review matrix for a RepoCIConfig.
// If Reviews is set, uses it directly. Otherwise falls back to
// the cross-product of Agents x ReviewTypes (which may be empty,
// meaning "use global").
func (c *RepoCIConfig) ResolvedReviewMatrix() []AgentReviewType {
	if c.Reviews != nil {
		// Reviews map is configured — return the resolved matrix
		// even when empty (signals "disable reviews for this repo").
		m := reviewsMapToMatrix(c.Reviews)
		if m == nil {
			return []AgentReviewType{}
		}
		return m
	}
	return nil
}

// reviewsMapToMatrix converts a Reviews map to a sorted slice of
// AgentReviewType pairs. Agents are sorted alphabetically; review
// types preserve their declared order within each agent.
func reviewsMapToMatrix(
	reviews map[string][]string,
) []AgentReviewType {
	agents := make([]string, 0, len(reviews))
	for agent := range reviews {
		agents = append(agents, agent)
	}
	slices.Sort(agents)

	var matrix []AgentReviewType
	for _, agent := range agents {
		for _, rt := range reviews[agent] {
			matrix = append(matrix, AgentReviewType{
				Agent:      agent,
				ReviewType: rt,
			})
		}
	}
	return matrix
}

// ResolvedThrottleInterval returns the minimum time between reviews
// of the same PR. Defaults to 1h if empty or unparseable.
// Returns 0 (disabled) if explicitly set to "0".
func (c *CIConfig) ResolvedThrottleInterval() time.Duration {
	if c.ThrottleInterval == "" {
		return time.Hour
	}
	if c.ThrottleInterval == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.ThrottleInterval)
	if err != nil || d < 0 {
		return time.Hour
	}
	return d
}

// ResolvedBatchTimeout returns how long to wait for all batch jobs
// before posting early with available results. Default: 3 minutes.
// Returns 0 (disabled) if explicitly set to "0".
func (c *CIConfig) ResolvedBatchTimeout() time.Duration {
	if c.BatchTimeout == "" {
		return 3 * time.Minute
	}
	if c.BatchTimeout == "0" {
		return 0
	}
	d, err := time.ParseDuration(c.BatchTimeout)
	if err != nil || d < 0 {
		return 3 * time.Minute
	}
	return d
}

// IsThrottleBypassed reports whether the given GitHub login is in
// the ThrottleBypassUsers list. Comparison is case-insensitive.
func (c *CIConfig) IsThrottleBypassed(login string) bool {
	lower := strings.ToLower(login)
	for _, u := range c.ThrottleBypassUsers {
		if strings.ToLower(u) == lower {
			return true
		}
	}
	return false
}

// ResolvedMaxRepos returns the maximum number of repos to poll.
// Defaults to 100 if not set or non-positive.
func (c *CIConfig) ResolvedMaxRepos() int {
	if c.MaxRepos > 0 {
		return c.MaxRepos
	}
	return 100
}

// SyncConfig holds configuration for PostgreSQL sync
type SyncConfig struct {
	// Enabled enables sync to PostgreSQL
	Enabled bool `toml:"enabled"`

	// PostgresURL is the connection string for PostgreSQL.
	// Supports environment variable expansion via ${VAR} syntax.
	PostgresURL string `toml:"postgres_url" sensitive:"true"`

	// Interval is how often to sync (e.g., "5m", "1h"). Default: 1h
	Interval string `toml:"interval"`

	// MachineName is a friendly name for this machine (optional)
	MachineName string `toml:"machine_name"`

	// ConnectTimeout is the connection timeout (e.g., "5s"). Default: 5s
	ConnectTimeout string `toml:"connect_timeout"`

	// RepoNames provides custom display names for synced repos by identity.
	// Example: {"git@github.com:org/repo.git": "my-project"}
	RepoNames map[string]string `toml:"repo_names"`
}

// PostgresURLExpanded returns the PostgreSQL URL with environment variables expanded.
// Returns empty string if URL is not set.
func (c *SyncConfig) PostgresURLExpanded() string {
	if c.PostgresURL == "" {
		return ""
	}
	return os.ExpandEnv(c.PostgresURL)
}

// GetRepoDisplayName returns the configured display name for a repo identity,
// or empty string if no override is configured.
func (c *SyncConfig) GetRepoDisplayName(identity string) string {
	if c == nil || c.RepoNames == nil {
		return ""
	}
	return c.RepoNames[identity]
}

// Validate checks the sync configuration for common issues.
// Returns a list of warnings (non-fatal issues).
func (c *SyncConfig) Validate() []string {
	var warnings []string

	if !c.Enabled {
		return warnings
	}

	if c.PostgresURL == "" {
		warnings = append(warnings, "sync.enabled is true but sync.postgres_url is not set")
		return warnings
	}

	// Check for environment variable references where the var is not set
	// os.ExpandEnv replaces ${VAR} with empty string if VAR is not set
	if strings.Contains(c.PostgresURL, "${") {
		re := regexp.MustCompile(`\$\{([^}]+)\}`)
		matches := re.FindAllStringSubmatch(c.PostgresURL, -1)
		for _, match := range matches {
			if len(match) > 1 {
				varName := match[1]
				if os.Getenv(varName) == "" {
					warnings = append(warnings, "sync.postgres_url may contain unexpanded environment variables")
					break // Only one warning needed
				}
			}
		}
	}

	return warnings
}

// RepoCIConfig holds per-repo CI overrides (used by the CI poller for this repo).
// These override the global [ci] settings when reviewing this specific repo.
type RepoCIConfig struct {
	// Agents overrides the list of agents for CI reviews of this repo.
	Agents []string `toml:"agents" comment:"Override the agents used by CI for this repo."`

	// ReviewTypes overrides the list of review types for CI reviews of this repo.
	ReviewTypes []string `toml:"review_types" comment:"Override the review types used by CI for this repo."`

	// Reviews maps agent names to review type lists. When set, replaces
	// the ReviewTypes x Agents cross-product for this repo.
	Reviews map[string][]string `toml:"reviews" comment:"Explicit CI review matrix for this repo: agent name to review types."`

	// Reasoning overrides the reasoning level for CI reviews.
	Reasoning string `toml:"reasoning" comment:"Override the CI reasoning level for this repo: fast, standard, medium, thorough, or maximum."`

	// MinSeverity overrides the minimum severity filter for CI synthesis.
	MinSeverity string `toml:"min_severity" comment:"Override the minimum CI severity included in synthesized output."`

	// UpsertComments overrides the global ci.upsert_comments setting.
	// Use a pointer so we can distinguish "not set" from "explicitly false".
	UpsertComments *bool `toml:"upsert_comments" comment:"Override whether CI updates an existing PR comment instead of creating a new one."`
}

// RepoConfig holds per-repo overrides
type RepoConfig struct {
	Agent                      string   `toml:"agent" comment:"Default agent for this repo when no workflow-specific agent is set."`
	Model                      string   `toml:"model" comment:"Default model for this repo when no workflow-specific model is set."` // Model for agents (format varies by agent)
	BackupAgent                string   `toml:"backup_agent" comment:"Backup agent for this repo if the primary agent fails."`
	BackupModel                string   `toml:"backup_model" comment:"Backup model for this repo if the primary model fails."`
	ReviewContextCount         int      `toml:"review_context_count" comment:"Number of related reviews to include as context for this repo."`
	ReviewGuidelines           string   `toml:"review_guidelines" comment:"Extra review instructions added to prompts for this repo."`
	JobTimeoutMinutes          int      `toml:"job_timeout_minutes" comment:"Override the review job timeout in minutes for this repo."`
	ExcludedBranches           []string `toml:"excluded_branches" comment:"Branches that should be skipped for automatic review in this repo."`
	ExcludedCommitPatterns     []string `toml:"excluded_commit_patterns" comment:"Commit message substrings that should skip review for this repo."`
	DisplayName                string   `toml:"display_name" comment:"Display name shown for this repo in the TUI and output."`
	ReviewReasoning            string   `toml:"review_reasoning" comment:"Reasoning level for reviews in this repo: fast, standard, medium, thorough, or maximum."`
	RefineReasoning            string   `toml:"refine_reasoning" comment:"Reasoning level for refine in this repo: fast, standard, medium, thorough, or maximum."`
	FixReasoning               string   `toml:"fix_reasoning" comment:"Reasoning level for fix in this repo: fast, standard, medium, thorough, or maximum."`
	FixMinSeverity             string   `toml:"fix_min_severity" comment:"Minimum severity for fix in this repo: critical, high, medium, or low."`     // Minimum severity for fix: critical, high, medium, low
	RefineMinSeverity          string   `toml:"refine_min_severity" comment:"Minimum severity for refine in this repo: critical, high, medium, low."`  // Minimum severity for refine: critical, high, medium, low
	ReviewMinSeverity          string   `toml:"review_min_severity" comment:"Minimum severity for reviews in this repo: critical, high, medium, low."` // Minimum severity for review: critical, high, medium, low
	ExcludePatterns            []string `toml:"exclude_patterns" comment:"Filenames or glob patterns to exclude from review diffs for this repo."`
	SnapshotDir                string   `toml:"snapshot_dir" comment:"Repo-local directory for temporary oversized diff snapshots."`
	PostCommitReview           string   `toml:"post_commit_review" comment:"Automatic post-commit review mode for this repo: commit or branch."` // "commit" (default) or "branch"
	ReuseReviewSession         *bool    `toml:"reuse_review_session"`
	ReuseReviewSessionLookback int      `toml:"reuse_review_session_lookback"` // 0 means no candidate cap

	// CI-specific overrides (used by CI poller for this repo)
	CI RepoCIConfig `toml:"ci"`

	// Auto design review overrides for this repo (opt-in)
	AutoDesignReview AutoDesignReviewRepoConfig `toml:"auto_design_review"`

	// Workflow-specific agent/model configuration
	ReviewAgent           string `toml:"review_agent" comment:"Agent override for standard review in this repo."`
	ReviewAgentFast       string `toml:"review_agent_fast" comment:"Agent override for fast review in this repo."`
	ReviewAgentStandard   string `toml:"review_agent_standard" comment:"Agent override for standard review in this repo."`
	ReviewAgentMedium     string `toml:"review_agent_medium" comment:"Agent override for medium review in this repo."`
	ReviewAgentThorough   string `toml:"review_agent_thorough" comment:"Agent override for thorough review in this repo."`
	ReviewAgentMaximum    string `toml:"review_agent_maximum" comment:"Agent override for maximum review in this repo."`
	RefineAgent           string `toml:"refine_agent" comment:"Agent override for refine in this repo."`
	RefineAgentFast       string `toml:"refine_agent_fast" comment:"Agent override for fast refine in this repo."`
	RefineAgentStandard   string `toml:"refine_agent_standard" comment:"Agent override for standard refine in this repo."`
	RefineAgentMedium     string `toml:"refine_agent_medium" comment:"Agent override for medium refine in this repo."`
	RefineAgentThorough   string `toml:"refine_agent_thorough" comment:"Agent override for thorough refine in this repo."`
	RefineAgentMaximum    string `toml:"refine_agent_maximum" comment:"Agent override for maximum refine in this repo."`
	ReviewModel           string `toml:"review_model" comment:"Model override for standard review in this repo."`
	ReviewModelFast       string `toml:"review_model_fast" comment:"Model override for fast review in this repo."`
	ReviewModelStandard   string `toml:"review_model_standard" comment:"Model override for standard review in this repo."`
	ReviewModelMedium     string `toml:"review_model_medium" comment:"Model override for medium review in this repo."`
	ReviewModelThorough   string `toml:"review_model_thorough" comment:"Model override for thorough review in this repo."`
	ReviewModelMaximum    string `toml:"review_model_maximum" comment:"Model override for maximum review in this repo."`
	RefineModel           string `toml:"refine_model" comment:"Model override for standard refine in this repo."`
	RefineModelFast       string `toml:"refine_model_fast" comment:"Model override for fast refine in this repo."`
	RefineModelStandard   string `toml:"refine_model_standard" comment:"Model override for standard refine in this repo."`
	RefineModelMedium     string `toml:"refine_model_medium" comment:"Model override for medium refine in this repo."`
	RefineModelThorough   string `toml:"refine_model_thorough" comment:"Model override for thorough refine in this repo."`
	RefineModelMaximum    string `toml:"refine_model_maximum" comment:"Model override for maximum refine in this repo."`
	FixAgent              string `toml:"fix_agent" comment:"Agent override for fix in this repo."`
	FixAgentFast          string `toml:"fix_agent_fast" comment:"Agent override for fast fix in this repo."`
	FixAgentStandard      string `toml:"fix_agent_standard" comment:"Agent override for standard fix in this repo."`
	FixAgentMedium        string `toml:"fix_agent_medium" comment:"Agent override for medium fix in this repo."`
	FixAgentThorough      string `toml:"fix_agent_thorough" comment:"Agent override for thorough fix in this repo."`
	FixAgentMaximum       string `toml:"fix_agent_maximum" comment:"Agent override for maximum fix in this repo."`
	FixModel              string `toml:"fix_model" comment:"Model override for standard fix in this repo."`
	FixModelFast          string `toml:"fix_model_fast" comment:"Model override for fast fix in this repo."`
	FixModelStandard      string `toml:"fix_model_standard" comment:"Model override for standard fix in this repo."`
	FixModelMedium        string `toml:"fix_model_medium" comment:"Model override for medium fix in this repo."`
	FixModelThorough      string `toml:"fix_model_thorough" comment:"Model override for thorough fix in this repo."`
	FixModelMaximum       string `toml:"fix_model_maximum" comment:"Model override for maximum fix in this repo."`
	SecurityAgent         string `toml:"security_agent" comment:"Agent override for security review in this repo."`
	SecurityAgentFast     string `toml:"security_agent_fast" comment:"Agent override for fast security review in this repo."`
	SecurityAgentStandard string `toml:"security_agent_standard" comment:"Agent override for standard security review in this repo."`
	SecurityAgentMedium   string `toml:"security_agent_medium" comment:"Agent override for medium security review in this repo."`
	SecurityAgentThorough string `toml:"security_agent_thorough" comment:"Agent override for thorough security review in this repo."`
	SecurityAgentMaximum  string `toml:"security_agent_maximum" comment:"Agent override for maximum security review in this repo."`
	SecurityModel         string `toml:"security_model" comment:"Model override for standard security review in this repo."`
	SecurityModelFast     string `toml:"security_model_fast" comment:"Model override for fast security review in this repo."`
	SecurityModelStandard string `toml:"security_model_standard" comment:"Model override for standard security review in this repo."`
	SecurityModelMedium   string `toml:"security_model_medium" comment:"Model override for medium security review in this repo."`
	SecurityModelThorough string `toml:"security_model_thorough" comment:"Model override for thorough security review in this repo."`
	SecurityModelMaximum  string `toml:"security_model_maximum" comment:"Model override for maximum security review in this repo."`
	DesignAgent           string `toml:"design_agent" comment:"Agent override for design review in this repo."`
	DesignAgentFast       string `toml:"design_agent_fast" comment:"Agent override for fast design review in this repo."`
	DesignAgentStandard   string `toml:"design_agent_standard" comment:"Agent override for standard design review in this repo."`
	DesignAgentMedium     string `toml:"design_agent_medium" comment:"Agent override for medium design review in this repo."`
	DesignAgentThorough   string `toml:"design_agent_thorough" comment:"Agent override for thorough design review in this repo."`
	DesignAgentMaximum    string `toml:"design_agent_maximum" comment:"Agent override for maximum design review in this repo."`
	DesignModel           string `toml:"design_model" comment:"Model override for standard design review in this repo."`
	DesignModelFast       string `toml:"design_model_fast" comment:"Model override for fast design review in this repo."`
	DesignModelStandard   string `toml:"design_model_standard" comment:"Model override for standard design review in this repo."`
	DesignModelMedium     string `toml:"design_model_medium" comment:"Model override for medium design review in this repo."`
	DesignModelThorough   string `toml:"design_model_thorough" comment:"Model override for thorough design review in this repo."`
	DesignModelMaximum    string `toml:"design_model_maximum" comment:"Model override for maximum design review in this repo."`

	// Classify workflow (per-repo overrides)
	ClassifyAgent       string `toml:"classify_agent" comment:"Override classifier agent for this repo."`
	ClassifyModel       string `toml:"classify_model" comment:"Override classifier model for this repo."`
	ClassifyReasoning   string `toml:"classify_reasoning" comment:"Override classifier reasoning for this repo."`
	ClassifyBackupAgent string `toml:"classify_backup_agent" comment:"Override classifier backup agent for this repo."`
	ClassifyBackupModel string `toml:"classify_backup_model" comment:"Override classifier backup model for this repo."`

	// Backup agents for failover
	ReviewBackupAgent   string `toml:"review_backup_agent" comment:"Backup agent for review in this repo."`
	RefineBackupAgent   string `toml:"refine_backup_agent" comment:"Backup agent for refine in this repo."`
	FixBackupAgent      string `toml:"fix_backup_agent" comment:"Backup agent for fix in this repo."`
	SecurityBackupAgent string `toml:"security_backup_agent" comment:"Backup agent for security review in this repo."`
	DesignBackupAgent   string `toml:"design_backup_agent" comment:"Backup agent for design review in this repo."`

	// Backup models for failover (used when failing over to backup agent)
	ReviewBackupModel   string `toml:"review_backup_model" comment:"Backup model for review in this repo."`
	RefineBackupModel   string `toml:"refine_backup_model" comment:"Backup model for refine in this repo."`
	FixBackupModel      string `toml:"fix_backup_model" comment:"Backup model for fix in this repo."`
	SecurityBackupModel string `toml:"security_backup_model" comment:"Backup model for security review in this repo."`
	DesignBackupModel   string `toml:"design_backup_model" comment:"Backup model for design review in this repo."`

	// Behavior
	AutoClosePassingReviews *bool `toml:"auto_close_passing_reviews" comment:"Automatically close reviews that pass with no findings in this repo."`
	ShowClassifyJobs        *bool `toml:"show_classify_jobs" comment:"Override whether the TUI queue shows auto-design-review classifier rows for this repo. Omit to inherit."`

	// Hooks configuration (per-repo)
	Hooks []HookConfig `toml:"hooks"`

	// Analysis settings
	MaxPromptSize int `toml:"max_prompt_size" comment:"Maximum prompt size for this repo before falling back to file paths."` // Max prompt size in bytes before falling back to paths (overrides global default)

	// ACP (Agent Client Protocol) configuration for this repo
	ACP *ACPAgentConfig `toml:"acp"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	cfg := &Config{
		ServerAddr:         "127.0.0.1:7373",
		MaxWorkers:         4,
		ReviewContextCount: 3,
		DefaultAgent:       "codex",
		JobTimeoutMinutes:  30,
		CodexCmd:           "codex",
		ClaudeCodeCmd:      "claude",
		CursorCmd:          "agent",
		PiCmd:              "pi",
		OpenCodeCmd:        "opencode",
		MouseEnabled:       true,
		Agent: AgentConfig{
			Codex: CodexConfig{
				DisableReviewSkills:    true,
				IgnoreReviewUserConfig: true,
			},
		},
	}
	cfg.CI.ThrottleBypassUsers = []string{
		"wesm", "mariusvniekerk",
	}
	return cfg
}

// DataDir returns the roborev data directory.
// Uses ROBOREV_DATA_DIR env var if set, otherwise ~/.roborev
func DataDir() string {
	if dir := os.Getenv("ROBOREV_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".roborev")
}

// GlobalConfigPath returns the path to the global config file
func GlobalConfigPath() string {
	return filepath.Join(DataDir(), "config.toml")
}

// LoadGlobal loads the global configuration from the default path
func LoadGlobal() (*Config, error) {
	return LoadGlobalFrom(GlobalConfigPath())
}

// LoadGlobalFrom loads the global configuration from a specific path
func LoadGlobalFrom(path string) (*Config, error) {
	cfg := DefaultConfig()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}

	md, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, err
	}

	// Migrate deprecated config keys
	cfg.migrateDeprecated(md)

	if err := cfg.CI.NormalizeInstallations(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return cfg, nil
}

// HiddenColumnsNoneSentinel is saved to hidden_columns when the
// user explicitly wants all columns visible. This distinguishes
// "hide nothing" from "never configured" (nil/empty slice).
const HiddenColumnsNoneSentinel = "_"

// migrateDeprecated promotes deprecated config keys to their
// replacements so the rest of the codebase only reads the new names.
// Uses TOML metadata to avoid overriding explicitly-set new keys.
func (c *Config) migrateDeprecated(md toml.MetaData) {
	// hide_addressed_by_default → hide_closed_by_default
	// Only promote if the new key wasn't explicitly set in the file.
	if c.HideAddressedByDefault && !md.IsDefined("hide_closed_by_default") {
		c.HideClosedByDefault = true
	}
	c.HideAddressedByDefault = false

	// Preserve explicit hidden_columns = [] as "hide nothing" before
	// the rename filter runs — otherwise a stale list that becomes
	// empty after filtering would be misinterpreted as "hide nothing"
	// instead of falling through to defaults.
	explicitlyEmpty := md.IsDefined("hidden_columns") &&
		len(c.HiddenColumns) == 0

	// hidden_columns: "handled"/"done" → "closed"
	filtered := c.HiddenColumns[:0]
	for _, name := range c.HiddenColumns {
		switch name {
		case "handled", "done":
			filtered = append(filtered, "closed")
		default:
			filtered = append(filtered, name)
		}
	}
	c.HiddenColumns = filtered

	if explicitlyEmpty {
		c.HiddenColumns = []string{HiddenColumnsNoneSentinel}
	}
}

// LoadRepoConfig loads per-repo config from .roborev.toml
func LoadRepoConfig(repoPath string) (*RepoConfig, error) {
	path := filepath.Join(repoPath, ".roborev.toml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil // No repo config
	}

	var cfg RepoConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// ValidateRepoConfig returns any repo-config load or parse error for repoPath.
// Missing repo config is treated as valid.
func ValidateRepoConfig(repoPath string) error {
	if strings.TrimSpace(repoPath) == "" {
		return nil
	}
	_, err := LoadRepoConfig(repoPath)
	return err
}

// ResolvePostCommitReview returns the post-commit review mode for a repo.
// Returns "branch" when configured, otherwise "commit" (the default).
func ResolvePostCommitReview(repoPath string) string {
	cfg, err := LoadRepoConfig(repoPath)
	if err != nil || cfg == nil {
		return "commit"
	}
	if cfg.PostCommitReview == "branch" {
		return "branch"
	}
	return "commit"
}

// ResolveReuseReviewSession returns whether reviews should try to resume a
// prior session from the same branch. Priority: repo > global > default false.
func ResolveReuseReviewSession(repoPath string, globalCfg *Config) bool {
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && repoCfg.ReuseReviewSession != nil {
		return *repoCfg.ReuseReviewSession
	}
	if globalCfg != nil && globalCfg.ReuseReviewSession != nil {
		return *globalCfg.ReuseReviewSession
	}
	return false
}

// ResolveDisableCodexReviewSkills returns whether Codex review jobs should
// suppress Codex skill instructions. Priority: global > default true.
func ResolveDisableCodexReviewSkills(_ string, globalCfg *Config) bool {
	if globalCfg != nil {
		return globalCfg.Agent.Codex.DisableReviewSkills
	}
	return true
}

// ResolveIgnoreCodexReviewUserConfig returns whether Codex review jobs should
// pass --ignore-user-config. Priority: global > default true.
func ResolveIgnoreCodexReviewUserConfig(_ string, globalCfg *Config) bool {
	if globalCfg != nil {
		return globalCfg.Agent.Codex.IgnoreReviewUserConfig
	}
	return true
}

// ResolveReuseReviewSessionLookback returns how many recent reusable-session
// candidates should be considered. Priority: repo > global > default unlimited.
// Non-positive values disable the cap.
func ResolveReuseReviewSessionLookback(repoPath string, globalCfg *Config) int {
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		if rawRepo, rawErr := LoadRawRepo(repoPath); rawErr == nil && IsKeyInTOMLFile(rawRepo, "reuse_review_session_lookback") {
			if repoCfg.ReuseReviewSessionLookback <= 0 {
				return 0
			}
			return repoCfg.ReuseReviewSessionLookback
		}
	}
	if globalCfg != nil && globalCfg.ReuseReviewSessionLookback > 0 {
		return globalCfg.ReuseReviewSessionLookback
	}
	return 0
}

// LoadRepoConfigFromRef loads per-repo config from .roborev.toml at a
// specific git ref (e.g., a commit SHA or "origin/main"). Returns
// (nil, nil) if the file doesn't exist at that ref. Returns an error
// for unexpected git failures (bad repo, corrupted objects, etc.).
func LoadRepoConfigFromRef(repoPath, ref string) (*RepoConfig, error) {
	data, err := git.ReadFile(repoPath, ref, ".roborev.toml")
	if err != nil {
		errMsg := err.Error()
		// git show emits these specific patterns when the path is missing:
		//   "path '...' does not exist in '...'"
		//   "path '...' exists on disk, but not in '...'"
		if strings.Contains(errMsg, "does not exist in") ||
			strings.Contains(errMsg, "exists on disk, but not in") {
			return nil, nil
		}
		return nil, fmt.Errorf("read .roborev.toml at %s: %w", ref, err)
	}

	var cfg RepoConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, &ConfigParseError{Ref: ref, Err: err}
	}
	return &cfg, nil
}

// resolve returns the first non-zero value from the candidates, or defaultVal
// if all candidates are zero. This encapsulates the standard precedence logic
// (explicit > repo > global > default) used throughout config resolution.
func resolve[T comparable](defaultVal T, candidates ...T) T {
	var zero T
	for _, v := range candidates {
		if v != zero {
			return v
		}
	}
	return defaultVal
}

// resolveSlice returns the first non-empty slice from candidates, or defaultVal.
func resolveSlice[T any](defaultVal []T, candidates ...[]T) []T {
	for _, v := range candidates {
		if len(v) > 0 {
			return v
		}
	}
	return defaultVal
}

// resolveBool returns the first non-nil boolean from candidates, or defaultVal.
func resolveBool(defaultVal bool, candidates ...*bool) bool {
	for _, v := range candidates {
		if v != nil {
			return *v
		}
	}
	return defaultVal
}

func splitTrimmedCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func singleTrimmedValue(value string) []string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return []string{trimmed}
	}
	return nil
}

func resolveNormalized(
	defaultVal string,
	normalize func(string) (string, error),
	explicit string,
	candidates ...string,
) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return normalize(explicit)
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if normalized, err := normalize(candidate); err == nil && normalized != "" {
			return normalized, nil
		}
	}
	return defaultVal, nil
}

// ResolveAgent determines which agent to use based on config priority:
// 1. Explicit agent parameter (if non-empty)
// 2. Per-repo config
// 3. Global config
// 4. Default ("codex")
func ResolveAgent(explicit string, repoPath string, globalCfg *Config) string {
	var repoVal string
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		repoVal = repoCfg.Agent
	}
	var globalVal string
	if globalCfg != nil {
		globalVal = globalCfg.DefaultAgent
	}
	return resolve("codex", explicit, repoVal, globalVal)
}

// ResolveCIAgents determines which agents to use for CI review execution.
// Priority: explicit CSV flag > repo [ci].agents > global [ci].agents > [""].
func ResolveCIAgents(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	if explicit != "" {
		return splitTrimmedCSV(explicit)
	}
	var repoAgents []string
	if repoCfg != nil {
		repoAgents = repoCfg.CI.Agents
	}
	var globalAgents []string
	if globalCfg != nil {
		globalAgents = globalCfg.CI.Agents
	}
	return resolveSlice([]string{""}, repoAgents, globalAgents)
}

// ResolveCIReviewTypes determines which review types to use for CI review execution.
// Priority: explicit CSV flag > repo [ci].review_types > global [ci].review_types > ["security"].
func ResolveCIReviewTypes(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	if explicit != "" {
		return splitTrimmedCSV(explicit)
	}
	var repoTypes []string
	if repoCfg != nil {
		repoTypes = repoCfg.CI.ReviewTypes
	}
	var globalTypes []string
	if globalCfg != nil {
		globalTypes = globalCfg.CI.ReviewTypes
	}
	return resolveSlice([]string{ReviewTypeSecurity}, repoTypes, globalTypes)
}

// ResolveCIReasoning determines the reasoning level for CI review execution.
// Priority: explicit > repo [ci].reasoning > "thorough".
func ResolveCIReasoning(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (string, error) {
	var repoVal string
	if repoCfg != nil {
		repoVal = repoCfg.CI.Reasoning
	}
	_ = globalCfg
	return resolveNormalized("thorough", NormalizeReasoning, explicit, repoVal)
}

// ResolveCIMinSeverity determines the synthesis severity filter for CI review execution.
// Priority: explicit > repo [ci].min_severity > global [ci].min_severity > "".
func ResolveCIMinSeverity(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (string, error) {
	var repoVal string
	if repoCfg != nil {
		repoVal = repoCfg.CI.MinSeverity
	}
	var globalVal string
	if globalCfg != nil {
		globalVal = globalCfg.CI.MinSeverity
	}
	return resolveNormalized("", NormalizeMinSeverity, explicit, repoVal, globalVal)
}

// ResolveCISynthesisAgent determines the synthesis agent for CI review execution.
// Priority: explicit > global [ci].synthesis_agent > "".
func ResolveCISynthesisAgent(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) string {
	var globalVal string
	if globalCfg != nil {
		globalVal = strings.TrimSpace(globalCfg.CI.SynthesisAgent)
	}
	_ = repoCfg
	return resolve("", strings.TrimSpace(explicit), globalVal)
}

// ResolveCIUpsertComments determines whether CI should update an existing PR comment.
// Priority: repo [ci].upsert_comments > global [ci].upsert_comments > false.
func ResolveCIUpsertComments(
	repoCfg *RepoConfig,
	globalCfg *Config,
) bool {
	var repoVal *bool
	if repoCfg != nil {
		repoVal = repoCfg.CI.UpsertComments
	}
	var globalVal *bool
	if globalCfg != nil {
		globalVal = &globalCfg.CI.UpsertComments
	}
	return resolveBool(false, repoVal, globalVal)
}

// ResolveCIWorkflowAgents determines which agents to encode into a generated CI workflow.
// Priority: explicit CSV flag > repo [ci].agents > repo agent > global [ci].agents > global default_agent > ["codex"].
func ResolveCIWorkflowAgents(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	if explicit != "" {
		return splitTrimmedCSV(explicit)
	}
	var repoCIAgents []string
	var repoAgent []string
	if repoCfg != nil {
		repoCIAgents = repoCfg.CI.Agents
		repoAgent = singleTrimmedValue(repoCfg.Agent)
	}
	var globalCIAgents []string
	var globalAgent []string
	if globalCfg != nil {
		globalCIAgents = globalCfg.CI.Agents
		globalAgent = singleTrimmedValue(globalCfg.DefaultAgent)
	}
	return resolveSlice(
		[]string{"codex"},
		repoCIAgents,
		repoAgent,
		globalCIAgents,
		globalAgent,
	)
}

// clampPositive returns v if v > 0, otherwise 0.
func clampPositive(v int) int {
	if v > 0 {
		return v
	}
	return 0
}

// ResolveJobTimeout determines job timeout based on config priority:
// 1. Per-repo config (if set and > 0)
// 2. Global config (if set and > 0)
// 3. Default (30 minutes)
func ResolveJobTimeout(repoPath string, globalCfg *Config) int {
	var repoVal int
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		repoVal = clampPositive(repoCfg.JobTimeoutMinutes)
	}
	var globalVal int
	if globalCfg != nil {
		globalVal = clampPositive(globalCfg.JobTimeoutMinutes)
	}
	return resolve(30, repoVal, globalVal)
}

// ResolveAutoClosePassingReviews returns whether passing reviews should
// be automatically closed. Per-repo config overrides global.
func ResolveAutoClosePassingReviews(repoPath string, globalCfg *Config) bool {
	var repoVal *bool
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		repoVal = repoCfg.AutoClosePassingReviews
	}
	var globalVal bool
	if globalCfg != nil {
		globalVal = globalCfg.AutoClosePassingReviews
	}
	return resolveBool(globalVal, repoVal)
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

// ResolveExcludePatterns returns the merged exclude patterns from
// repo config and global config. Repo patterns are read from the
// default branch (like review guidelines) to prevent untrusted
// branches from suppressing files in reviews. Falls back to the
// filesystem config only when no default branch config exists.
// Global patterns are appended after repo patterns (deduplicated).
//
// Security reviews skip repo-level patterns entirely so a
// compromised default branch cannot suppress files from review.
func ResolveExcludePatterns(
	repoPath string, globalCfg *Config, reviewType string,
) []string {
	var repo []string
	if reviewType != "security" {
		repo = loadRepoExcludePatterns(repoPath)
	}
	var global []string
	if globalCfg != nil {
		global = globalCfg.ExcludePatterns
	}
	if len(repo) == 0 && len(global) == 0 {
		return nil
	}
	return mergePatterns(repo, global)
}

// ResolveExcludePatternsLocal is like ResolveExcludePatterns but
// reads repo config from the working tree instead of the default
// branch. Use this for dirty reviews where the user is reviewing
// local changes and expects local config to apply.
func ResolveExcludePatternsLocal(
	repoPath string, globalCfg *Config, reviewType string,
) []string {
	var repo []string
	if reviewType != "security" {
		if fsCfg, err := LoadRepoConfig(repoPath); err == nil && fsCfg != nil {
			repo = fsCfg.ExcludePatterns
		}
	}
	var global []string
	if globalCfg != nil {
		global = globalCfg.ExcludePatterns
	}
	if len(repo) == 0 && len(global) == 0 {
		return nil
	}
	return mergePatterns(repo, global)
}

// loadRepoExcludePatterns reads exclude_patterns from the default
// branch's .roborev.toml, falling back to the filesystem config
// when no default branch config exists (e.g., no remote, or
// .roborev.toml not yet committed). This mirrors loadGuidelines
// to prevent untrusted branches from controlling review scope.
func loadRepoExcludePatterns(repoPath string) []string {
	if defaultBranch, err := git.GetDefaultBranch(repoPath); err == nil {
		cfg, err := LoadRepoConfigFromRef(repoPath, defaultBranch)
		if err != nil {
			if IsConfigParseError(err) {
				return nil
			}
			// Fall through to filesystem
		} else if cfg != nil {
			return cfg.ExcludePatterns
		}
	}
	if fsCfg, err := LoadRepoConfig(repoPath); err == nil && fsCfg != nil {
		return fsCfg.ExcludePatterns
	}
	return nil
}

func mergePatterns(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	merged := make([]string, 0, len(a)+len(b))
	for _, list := range [2][]string{a, b} {
		for _, p := range list {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			merged = append(merged, p)
		}
	}
	return merged
}

// IsBranchExcluded checks if a branch should be excluded from reviews
func IsBranchExcluded(repoPath, branch string) bool {
	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil || repoCfg == nil {
		return false
	}

	return slices.Contains(repoCfg.ExcludedBranches, branch)
}

// IsCommitMessageExcluded checks if a commit should be excluded
// from reviews based on substring patterns configured in the
// repo's .roborev.toml.
func IsCommitMessageExcluded(repoPath, message string) bool {
	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil || repoCfg == nil {
		return false
	}
	return messageMatchesPatterns(
		message, repoCfg.ExcludedCommitPatterns,
	)
}

// AllCommitMessagesExcluded reports whether every message in the
// slice matches at least one excluded-commit pattern. Returns false
// when the slice is empty or the repo has no config.
func AllCommitMessagesExcluded(
	repoPath string, messages []string,
) bool {
	if len(messages) == 0 {
		return false
	}
	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil || repoCfg == nil {
		return false
	}
	for _, msg := range messages {
		if !messageMatchesPatterns(
			msg, repoCfg.ExcludedCommitPatterns,
		) {
			return false
		}
	}
	return true
}

// messageMatchesPatterns returns true when message contains at
// least one non-empty pattern (case-insensitive substring match).
func messageMatchesPatterns(
	message string, patterns []string,
) bool {
	lower := strings.ToLower(message)
	for _, pattern := range patterns {
		if pattern != "" &&
			strings.Contains(lower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// GetDisplayName returns the display name for a repo, or empty if not set
func GetDisplayName(repoPath string) string {
	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil || repoCfg == nil {
		return ""
	}
	return repoCfg.DisplayName
}

// Canonical review type names.
const (
	ReviewTypeDefault  = "default"
	ReviewTypeSecurity = "security"
	ReviewTypeDesign   = "design"
)

// IsDefaultReviewType returns true if the review type represents the standard
// (non-specialized) code review. The canonical name is "default"; "general"
// and "review" are accepted as backward-compatible aliases.
func IsDefaultReviewType(rt string) bool {
	return rt == "" || rt == ReviewTypeDefault ||
		rt == "general" || rt == "review"
}

// ValidateReviewTypes canonicalizes, validates, and deduplicates
// a list of review type strings. Aliases ("general", "review")
// are normalized to "default". Returns an error if any type is
// empty or unrecognized.
func ValidateReviewTypes(types []string) ([]string, error) {
	validSpecial := map[string]bool{
		ReviewTypeSecurity: true,
		ReviewTypeDesign:   true,
	}
	seen := make(map[string]bool, len(types))
	canonical := make([]string, 0, len(types))
	for _, rt := range types {
		if rt == "" {
			return nil, fmt.Errorf(
				"invalid review_type %q "+
					"(valid: default, security, design)", rt)
		}
		if IsDefaultReviewType(rt) {
			rt = ReviewTypeDefault
		} else if !validSpecial[rt] {
			return nil, fmt.Errorf(
				"invalid review_type %q "+
					"(valid: default, security, design)", rt)
		}
		if !seen[rt] {
			seen[rt] = true
			canonical = append(canonical, rt)
		}
	}
	return canonical, nil
}

// NormalizeReasoning validates and normalizes a reasoning level string.
// Returns the canonical form (maximum, thorough, medium, standard, fast) or an error if invalid.
// Returns empty string (no error) for empty input.
func NormalizeReasoning(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", nil
	}

	switch normalized {
	case "maximum", "max", "xhigh":
		return "maximum", nil
	case "thorough", "high":
		return "thorough", nil
	case "medium":
		return "medium", nil
	case "standard":
		return "standard", nil
	case "fast", "low":
		return "fast", nil
	default:
		return "", fmt.Errorf("invalid reasoning level: %q", value)
	}
}

// NormalizeMinSeverity validates and normalizes a minimum severity level string.
// Returns the canonical form (critical, high, medium, low) or an error if invalid.
// Returns empty string (no error) for empty input.
func NormalizeMinSeverity(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", nil
	}

	switch normalized {
	case "critical", "high", "medium", "low":
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid min_severity level: %q (valid: critical, high, medium, low)", value)
	}
}

// severityAbove maps a minimum severity to the instruction
// describing which levels to include.
var severityAbove = map[string]string{
	"critical": "Only include Critical findings.",
	"high":     "Only include High and Critical findings.",
	"medium":   "Only include Medium, High, and Critical findings.",
}

// SeverityThresholdMarker is output by agents when all findings in a
// review are below the configured minimum severity. The refine loop
// checks for this marker to distinguish "nothing above threshold"
// from "agent couldn't fix it."
const SeverityThresholdMarker = "SEVERITY_THRESHOLD_MET"

// IsMarkerOnlyOutput reports whether output is essentially the
// SeverityThresholdMarker by itself, allowing only whitespace and
// minimal markdown decoration: bold (**...** or __...__), italic
// (*...* or _..._), a fenced code block, a leading list bullet,
// and an optional trailing period. Any prose or other substantive
// content disqualifies the output, since we cannot reliably tell
// chatty narration from prose findings without severity labels.
//
// Callers that want to treat the marker as a "below threshold"
// signal should use this helper rather than substring matching,
// which is too easy to fool with marker-bearing prose findings.
func IsMarkerOnlyOutput(output string) bool {
	s := strings.TrimSpace(output)
	if s == "" {
		return false
	}

	// Strip a fenced code block if the output is wrapped in one.
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		if _, after, found := strings.Cut(rest, "\n"); found {
			s = after
		} else {
			s = rest
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	// Strip a leading list marker (- or *) followed by space.
	if len(s) >= 2 && (s[0] == '-' || s[0] == '*') && s[1] == ' ' {
		s = strings.TrimSpace(s[2:])
	}

	// Strip surrounding bold/italic markers. Bold forms (** and __)
	// are stripped before italic forms (* and _) so that **X** fully
	// unwraps in one pass rather than degrading to *X*.
	for _, wrap := range []string{"**", "__", "*", "_"} {
		if strings.HasPrefix(s, wrap) && strings.HasSuffix(s, wrap) && len(s) >= 2*len(wrap) {
			s = strings.TrimSpace(s[len(wrap) : len(s)-len(wrap)])
		}
	}

	// Strip a trailing period.
	s = strings.TrimSpace(strings.TrimSuffix(s, "."))

	// Collapse any internal whitespace models might inject
	sNoSpace := strings.ReplaceAll(s, " ", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\n", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\r", "")
	sNoSpace = strings.ReplaceAll(sNoSpace, "\t", "")
	return sNoSpace == SeverityThresholdMarker
}

// SeverityInstruction returns a prompt instruction telling the agent
// to focus only on findings at or above minSeverity. Returns "" for
// empty, "low", or unrecognized input (no filtering needed).
func SeverityInstruction(minSeverity string) string {
	instruction, ok := severityAbove[minSeverity]
	if !ok {
		return ""
	}
	return "Severity filter: " + instruction +
		" Ignore any findings below " + minSeverity +
		" severity." +
		" If ALL findings in the review are below " +
		minSeverity + " severity, output the exact text " +
		SeverityThresholdMarker +
		" and make no code changes.\n"
}

// ResolveReviewReasoning determines reasoning level for reviews.
// Priority: explicit > per-repo config > global config > default (thorough)
func ResolveReviewReasoning(explicit string, repoPath string, globalCfg *Config) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if err := validateRepoReasoningOverride(repoPath, func(cfg *RepoConfig) string {
			return cfg.ReviewReasoning
		}); err != nil {
			return "", err
		}
		return NormalizeReasoning(explicit)
	}

	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil {
		return "", err
	}
	if repoCfg != nil && strings.TrimSpace(repoCfg.ReviewReasoning) != "" {
		return NormalizeReasoning(repoCfg.ReviewReasoning)
	}

	if globalCfg != nil && strings.TrimSpace(globalCfg.ReviewReasoning) != "" {
		return NormalizeReasoning(globalCfg.ReviewReasoning)
	}

	return "thorough", nil // Default for reviews: deep analysis
}

// ResolveRefineReasoning determines reasoning level for refine.
// Priority: explicit > per-repo config > global config > default (standard)
func ResolveRefineReasoning(explicit string, repoPath string, globalCfg *Config) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if err := validateRepoReasoningOverride(repoPath, func(cfg *RepoConfig) string {
			return cfg.RefineReasoning
		}); err != nil {
			return "", err
		}
		return NormalizeReasoning(explicit)
	}

	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil {
		return "", err
	}
	if repoCfg != nil && strings.TrimSpace(repoCfg.RefineReasoning) != "" {
		return NormalizeReasoning(repoCfg.RefineReasoning)
	}

	if globalCfg != nil && strings.TrimSpace(globalCfg.RefineReasoning) != "" {
		return NormalizeReasoning(globalCfg.RefineReasoning)
	}

	return "standard", nil // Default for refine: balanced analysis
}

// ResolveFixReasoning determines reasoning level for fix.
// Priority: explicit > per-repo config > global config > default (standard)
func ResolveFixReasoning(explicit string, repoPath string, globalCfg *Config) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if err := validateRepoReasoningOverride(repoPath, func(cfg *RepoConfig) string {
			return cfg.FixReasoning
		}); err != nil {
			return "", err
		}
		return NormalizeReasoning(explicit)
	}

	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil {
		return "", err
	}
	if repoCfg != nil && strings.TrimSpace(repoCfg.FixReasoning) != "" {
		return NormalizeReasoning(repoCfg.FixReasoning)
	}

	if globalCfg != nil && strings.TrimSpace(globalCfg.FixReasoning) != "" {
		return NormalizeReasoning(globalCfg.FixReasoning)
	}

	return "standard", nil // Default for fix: balanced analysis
}

func validateRepoReasoningOverride(
	repoPath string,
	repoValue func(*RepoConfig) string,
) error {
	if strings.TrimSpace(repoPath) == "" {
		return nil
	}

	repoCfg, err := LoadRepoConfig(repoPath)
	// Entry points that must fail fast on malformed .roborev.toml call
	// ValidateRepoConfig separately. Here we only want to catch a parseable
	// but invalid workflow reasoning override before an explicit CLI value
	// silently masks it.
	if err != nil || repoCfg == nil {
		return nil
	}

	reasoning := strings.TrimSpace(repoValue(repoCfg))
	if reasoning == "" {
		return nil
	}

	_, err = NormalizeReasoning(reasoning)
	return err
}

// ResolveFixMinSeverity determines minimum severity for fix.
// Priority: explicit > per-repo config > global config > "" (no filter)
func ResolveFixMinSeverity(explicit string, repoPath string, globalCfg *Config) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return NormalizeMinSeverity(explicit)
	}
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && strings.TrimSpace(repoCfg.FixMinSeverity) != "" {
		return NormalizeMinSeverity(repoCfg.FixMinSeverity)
	}
	if globalCfg != nil && strings.TrimSpace(globalCfg.FixMinSeverity) != "" {
		return NormalizeMinSeverity(globalCfg.FixMinSeverity)
	}
	return "", nil
}

// ResolveRefineMinSeverity determines minimum severity for refine.
// Priority: explicit > per-repo config > global config > "" (no filter)
func ResolveRefineMinSeverity(explicit string, repoPath string, globalCfg *Config) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return NormalizeMinSeverity(explicit)
	}
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && strings.TrimSpace(repoCfg.RefineMinSeverity) != "" {
		return NormalizeMinSeverity(repoCfg.RefineMinSeverity)
	}
	if globalCfg != nil && strings.TrimSpace(globalCfg.RefineMinSeverity) != "" {
		return NormalizeMinSeverity(globalCfg.RefineMinSeverity)
	}
	return "", nil
}

// ResolveReviewMinSeverity determines minimum severity for review.
// Priority: explicit > per-repo config > global config > "" (no filter)
func ResolveReviewMinSeverity(explicit string, repoPath string, globalCfg *Config) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return NormalizeMinSeverity(explicit)
	}
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil && strings.TrimSpace(repoCfg.ReviewMinSeverity) != "" {
		return NormalizeMinSeverity(repoCfg.ReviewMinSeverity)
	}
	if globalCfg != nil && strings.TrimSpace(globalCfg.ReviewMinSeverity) != "" {
		return NormalizeMinSeverity(globalCfg.ReviewMinSeverity)
	}
	return "", nil
}

// severityRank returns a numeric rank for a severity level.
// Higher rank = stricter threshold (fewer findings pass).
func severityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// StricterSeverity returns whichever severity threshold is stricter
// (filters more). Empty string means "no filter" (least strict).
func StricterSeverity(a, b string) string {
	if severityRank(a) >= severityRank(b) {
		return a
	}
	return b
}

// ResolveModel determines which model to use based on config priority:
// 1. Explicit model parameter (if non-empty)
// 2. Per-repo config (model in .roborev.toml)
// 3. Global config (default_model in config.toml)
// 4. Default (empty string, agent uses its default)
func ResolveModel(explicit string, repoPath string, globalCfg *Config) string {
	var repoVal string
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		repoVal = strings.TrimSpace(repoCfg.Model)
	}
	var globalVal string
	if globalCfg != nil {
		globalVal = strings.TrimSpace(globalCfg.DefaultModel)
	}
	return resolve("", strings.TrimSpace(explicit), repoVal, globalVal)
}

// DefaultMaxPromptSize is the default maximum prompt size in bytes (200KB)
const DefaultMaxPromptSize = 200 * 1024
const DefaultSnapshotDir = ".roborev"

// ResolveMaxPromptSize determines the maximum prompt size based on config priority:
// 1. Per-repo config (max_prompt_size in .roborev.toml)
// 2. Global config (default_max_prompt_size in config.toml)
// 3. Default (200KB)
func ResolveMaxPromptSize(repoPath string, globalCfg *Config) int {
	var repoVal int
	if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
		repoVal = clampPositive(repoCfg.MaxPromptSize)
	}
	var globalVal int
	if globalCfg != nil {
		globalVal = clampPositive(globalCfg.DefaultMaxPromptSize)
	}
	return resolve(DefaultMaxPromptSize, repoVal, globalVal)
}

// ResolveSnapshotDir returns the absolute repo-local directory used for
// temporary oversized diff snapshots.
func ResolveSnapshotDir(repoPath string) (string, error) {
	dir := DefaultSnapshotDir
	if repoCfg, err := LoadRepoConfig(repoPath); err != nil {
		return "", err
	} else if repoCfg != nil && strings.TrimSpace(repoCfg.SnapshotDir) != "" {
		dir = repoCfg.SnapshotDir
	}
	dir = strings.TrimSpace(dir)
	if strings.ContainsFunc(dir, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", fmt.Errorf("snapshot_dir must not contain control characters: %q", dir)
	}
	clean := filepath.Clean(dir)
	if clean == "." || !filepath.IsLocal(clean) {
		return "", fmt.Errorf("snapshot_dir must be a relative path under the repo root: %s", dir)
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git"+string(filepath.Separator)) {
		return "", fmt.Errorf("snapshot_dir must not be inside .git: %s", dir)
	}
	return filepath.Join(repoPath, clean), nil
}

// ResolveAgentForWorkflow determines which agent to use based on workflow and level.
// Priority (Option A - layer wins first, then specificity):
// 1. CLI explicit
// 2. Repo {workflow}_agent_{level}
// 3. Repo {workflow}_agent
// 4. Repo agent
// 5. Global {workflow}_agent_{level}
// 6. Global {workflow}_agent
// 7. Global default_agent
// 8. "codex"
func ResolveAgentForWorkflow(cli, repoPath string, globalCfg *Config, workflow, level string) string {
	if s := strings.TrimSpace(cli); s != "" {
		return s
	}
	repoCfg, _ := LoadRepoConfig(repoPath)
	if s := getWorkflowValue(repoCfg, globalCfg, workflow, level, true); s != "" {
		return s
	}
	return "codex"
}

// ResolveModelForWorkflow determines which model to use based on workflow and level.
// Same priority as ResolveAgentForWorkflow, but returns empty string as default.
func ResolveModelForWorkflow(cli, repoPath string, globalCfg *Config, workflow, level string) string {
	if s := strings.TrimSpace(cli); s != "" {
		return s
	}
	repoCfg, _ := LoadRepoConfig(repoPath)
	return getWorkflowValue(repoCfg, globalCfg, workflow, level, false)
}

// ResolveWorkflowModel resolves a model from workflow-specific config only,
// skipping generic defaults (repo model, global default_model). Use this
// when the agent was overridden from a different source (e.g., CLI --agent)
// and the generic model is likely paired with a different default agent.
func ResolveWorkflowModel(repoPath string, globalCfg *Config, workflow, level string) string {
	repoCfg, _ := LoadRepoConfig(repoPath)
	if repoCfg != nil {
		if s := repoWorkflowField(repoCfg, workflow, level, false); s != "" {
			return s
		}
		if s := repoWorkflowField(repoCfg, workflow, "", false); s != "" {
			return s
		}
	}
	if globalCfg != nil {
		if s := globalWorkflowField(globalCfg, workflow, level, false); s != "" {
			return s
		}
		if s := globalWorkflowField(globalCfg, workflow, "", false); s != "" {
			return s
		}
	}
	return ""
}

// ResolveBackupAgentForWorkflow returns the backup agent for a workflow,
// or empty string if none is configured.
// Priority:
//  1. Repo {workflow}_backup_agent
//  2. Repo backup_agent (generic)
//  3. Global {workflow}_backup_agent
//  4. Global default_backup_agent
//  5. "" (no backup)
func ResolveBackupAgentForWorkflow(repoPath string, globalCfg *Config, workflow string) string {
	repoCfg, _ := LoadRepoConfig(repoPath)

	// Repo layer: workflow-specific > generic
	if repoCfg != nil {
		if s := lookupFieldByTag(reflect.ValueOf(*repoCfg), workflow+"_backup_agent"); s != "" {
			return s
		}
		if s := strings.TrimSpace(repoCfg.BackupAgent); s != "" {
			return s
		}
	}

	// Global layer: workflow-specific > default
	if globalCfg != nil {
		if s := lookupFieldByTag(reflect.ValueOf(*globalCfg), workflow+"_backup_agent"); s != "" {
			return s
		}
		if s := strings.TrimSpace(globalCfg.DefaultBackupAgent); s != "" {
			return s
		}
	}

	return ""
}

// ResolveBackupModelForWorkflow returns the backup model for a workflow,
// or empty string if none is configured.
// Priority:
//  1. Repo {workflow}_backup_model
//  2. Repo backup_model (generic)
//  3. Global {workflow}_backup_model
//  4. Global default_backup_model
//  5. "" (no override — agent uses its default)
func ResolveBackupModelForWorkflow(repoPath string, globalCfg *Config, workflow string) string {
	repoCfg, _ := LoadRepoConfig(repoPath)

	// Repo layer: workflow-specific > generic
	if repoCfg != nil {
		if s := lookupFieldByTag(reflect.ValueOf(*repoCfg), workflow+"_backup_model"); s != "" {
			return s
		}
		if s := strings.TrimSpace(repoCfg.BackupModel); s != "" {
			return s
		}
	}

	// Global layer: workflow-specific > default
	if globalCfg != nil {
		if s := lookupFieldByTag(reflect.ValueOf(*globalCfg), workflow+"_backup_model"); s != "" {
			return s
		}
		if s := strings.TrimSpace(globalCfg.DefaultBackupModel); s != "" {
			return s
		}
	}

	return ""
}

// ResolveACPAgentConfig returns the effective ACP agent configuration for a repo.
// Priority: repo [acp] config > global [acp] config > nil (no ACP config).
// Repo-level ACP config completely overrides global ACP config (no merging of individual fields).
func ResolveACPAgentConfig(repoPath string, globalCfg *Config) *ACPAgentConfig {
	// Only try repo config if repoPath is non-empty
	if repoPath != "" {
		repoCfg, err := LoadRepoConfig(repoPath)
		if err != nil {
			// Malformed repo config - fall through to global
			if IsConfigParseError(err) {
				// Parse error - skip repo config
			} else {
				repoCfg = nil
			}
		}
		if repoCfg != nil && repoCfg.ACP != nil {
			return repoCfg.ACP
		}
	}

	// Fall back to global config
	if globalCfg != nil && globalCfg.ACP != nil {
		return globalCfg.ACP
	}

	return nil
}

// lookupFieldByTag finds a struct field by its TOML tag and returns its trimmed value.
func lookupFieldByTag(v reflect.Value, key string) string {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("toml")
		if tag == "" {
			continue
		}
		if strings.Split(tag, ",")[0] == key {
			return strings.TrimSpace(v.Field(i).String())
		}
	}
	return ""
}

// getWorkflowValue looks up agent or model config following Option A priority.
func getWorkflowValue(repo *RepoConfig, global *Config, workflow, level string, isAgent bool) string {
	// Repo layer: level-specific > workflow-specific > generic
	if repo != nil {
		if s := repoWorkflowField(repo, workflow, level, isAgent); s != "" {
			return s
		}
		if s := repoWorkflowField(repo, workflow, "", isAgent); s != "" {
			return s
		}
		if isAgent && strings.TrimSpace(repo.Agent) != "" {
			return strings.TrimSpace(repo.Agent)
		}
		if !isAgent && strings.TrimSpace(repo.Model) != "" {
			return strings.TrimSpace(repo.Model)
		}
	}
	// Global layer: level-specific > workflow-specific > generic
	if global != nil {
		if s := globalWorkflowField(global, workflow, level, isAgent); s != "" {
			return s
		}
		if s := globalWorkflowField(global, workflow, "", isAgent); s != "" {
			return s
		}
		if isAgent && strings.TrimSpace(global.DefaultAgent) != "" {
			return strings.TrimSpace(global.DefaultAgent)
		}
		if !isAgent && strings.TrimSpace(global.DefaultModel) != "" {
			return strings.TrimSpace(global.DefaultModel)
		}
	}
	return ""
}

// workflowFieldKey builds the TOML key for a workflow field lookup.
// Examples: workflowFieldKey("review", "fast", true) => "review_agent_fast"
//
//	workflowFieldKey("review", "", true) => "review_agent"
func workflowFieldKey(workflow, level string, isAgent bool) string {
	kind := "model"
	if isAgent {
		kind = "agent"
	}
	if level == "" {
		return workflow + "_" + kind
	}
	return workflow + "_" + kind + "_" + level
}

// lookupWorkflowField retrieves a workflow field value from any struct using
// reflection and TOML tags. This replaces the former repoWorkflowField and
// globalWorkflowField switch statements with a single, tag-driven lookup that
// automatically supports new workflows/levels when fields are added.
func lookupWorkflowField(v reflect.Value, workflow, level string, isAgent bool) string {
	key := workflowFieldKey(workflow, level, isAgent)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("toml")
		if tag == "" {
			continue
		}
		if strings.Split(tag, ",")[0] == key {
			return strings.TrimSpace(v.Field(i).String())
		}
	}
	return ""
}

func repoWorkflowField(r *RepoConfig, workflow, level string, isAgent bool) string {
	if r == nil {
		return ""
	}
	return lookupWorkflowField(reflect.ValueOf(*r), workflow, level, isAgent)
}

func globalWorkflowField(g *Config, workflow, level string, isAgent bool) string {
	if g == nil {
		return ""
	}
	return lookupWorkflowField(reflect.ValueOf(*g), workflow, level, isAgent)
}

// SaveGlobal saves the global configuration
func SaveGlobal(cfg *Config) error {
	return SaveGlobalTo(GlobalConfigPath(), cfg)
}

// SaveGlobalTo saves the global configuration to a specific path.
func SaveGlobalTo(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := tomlv2.Marshal(cfg)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".roborev-config-*.toml")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// SaveRepoConfigTo saves a per-repo configuration to a specific path.
func SaveRepoConfigTo(path string, cfg *RepoConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := tomlv2.Marshal(cfg)
	if err != nil {
		return err
	}

	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".roborev-repo-config-*.toml")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// roborevIDPattern validates .roborev-id content.
// Must start with alphanumeric, then allows alphanumeric, dots, underscores, hyphens, colons, slashes, at-signs.
var roborevIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:@/-]*$`)

const roborevIDMaxLength = 256

// ValidateReporevID validates the content of a .roborev-id file.
// Returns empty string if valid, or an error message if invalid.
func ValidateRoborevID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "empty after trimming whitespace"
	}
	if len(id) > roborevIDMaxLength {
		return fmt.Sprintf("exceeds max length of %d characters", roborevIDMaxLength)
	}
	if !roborevIDPattern.MatchString(id) {
		return "invalid characters (must start with alphanumeric, then alphanumeric/._:@/-)"
	}
	return ""
}

// ReadRoborevID reads and validates the .roborev-id file from a repo.
// Returns the ID if valid, empty string if file doesn't exist or is invalid.
// If invalid, the error describes why.
func ReadRoborevID(repoPath string) (string, error) {
	path := filepath.Join(repoPath, ".roborev-id")
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read .roborev-id: %w", err)
	}

	id := strings.TrimSpace(string(content))
	if validationErr := ValidateRoborevID(id); validationErr != "" {
		return "", fmt.Errorf("invalid .roborev-id: %s", validationErr)
	}
	return id, nil
}

// ResolveRepoIdentity determines the unique identity for a repository.
// Resolution order:
// 1. .roborev-id file in repo root (if exists and valid)
// 2. Git remote "origin" URL
// 3. Any git remote URL
// 4. Fallback: local://{absolute_path}
//
// Note: Credentials are stripped from git remote URLs to prevent secrets from
// being persisted in the database or synced to PostgreSQL.
//
// The getRemoteURL parameter allows injection of git remote lookup for testing.
// Pass nil to use the default git.GetRemoteURL function.
func ResolveRepoIdentity(repoPath string, getRemoteURL func(repoPath, remoteName string) string) string {
	// 1. Try .roborev-id file
	id, err := ReadRoborevID(repoPath)
	if err == nil && id != "" {
		return id
	}
	// If .roborev-id exists but is invalid, fall through (logged at call site if needed)

	// 2 & 3. Try git remote URL (origin first, then any)
	if getRemoteURL == nil {
		getRemoteURL = git.GetRemoteURL
	}
	remoteURL := getRemoteURL(repoPath, "")
	if remoteURL != "" {
		// Strip credentials from URL to avoid persisting secrets
		return stripURLCredentials(remoteURL)
	}

	// 4. Fallback to local path
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		absPath = repoPath
	}
	return "local://" + absPath
}

// stripURLCredentials removes userinfo (username:password) from a URL.
// For non-URL strings (e.g., SSH URLs like git@github.com:user/repo.git),
// returns the original string unchanged.
func stripURLCredentials(rawURL string) string {
	// Try to parse as a standard URL
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// Not a valid URL, return as-is
		return rawURL
	}

	// If there's no scheme, it's likely an SCP-style URL (git@host:repo.git).
	// Strip any credentials (user:pass@host:repo → host:repo).
	if parsed.Scheme == "" {
		if _, after, ok := strings.Cut(rawURL, "@"); ok {
			return after
		}
		return rawURL
	}

	// If there's no userinfo, return as-is
	if parsed.User == nil {
		return rawURL
	}

	// Clear the userinfo
	parsed.User = nil
	return parsed.String()
}
