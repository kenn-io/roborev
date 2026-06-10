package config

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	tomlv2 "github.com/pelletier/go-toml/v2"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/git"
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
	Event    string   `toml:"event"`                // "review.failed", "review.completed", "review.*"
	Branches []string `toml:"branches"`             // optional branch globs (path.Match); empty = all branches
	Command  string   `toml:"command"`              // shell command with {var} templates
	Type     string   `toml:"type"`                 // "beads", "kata", or "webhook"; empty or "command" runs Command
	URL      string   `toml:"url" sensitive:"true"` // webhook destination URL when Type is "webhook"
	Project  string   `toml:"project"`              // kata: project name (defaults to .kata.toml binding)
	Labels   []string `toml:"labels"`               // kata: extra labels (roborev is always added)
	Priority *int     `toml:"priority"`             // kata: issue priority (0..4); nil = kata default
}

type AdvancedConfig struct {
	TasksEnabled bool `toml:"tasks_enabled" comment:"Enable the advanced Tasks workflow in the TUI."` // Enables advanced TUI tasks workflow
}

type AgentHookConfig struct {
	TurnThreshold         int    `toml:"turn_threshold" comment:"Stop hook threshold; 0 disables Stop-based prompting."`
	CommitThreshold       int    `toml:"commit_threshold" comment:"PostToolUse commit threshold; 0 disables commit-based prompting."`
	FailedReviewThreshold int    `toml:"failed_review_threshold" comment:"Open failed roborev review threshold; 0 disables review-based prompting."`
	Instruction           string `toml:"instruction" comment:"Instruction emitted when the agent hook decides a review/fix pass is needed."`
}

type CodexConfig struct {
	DisableReviewSkills    bool `toml:"disable_review_skills" comment:"Disable Codex skill instructions for review jobs."`
	IgnoreReviewUserConfig bool `toml:"ignore_review_user_config" comment:"Pass --ignore-user-config to Codex for review jobs."`
}

type PiConfig struct {
	JSONSchemaExtension string `toml:"jsonschemaextension" comment:"Pi extension source for classifier JSON schema output."`
}

type AgentConfig struct {
	Codex CodexConfig `toml:"codex"`
	Pi    PiConfig    `toml:"pi"`
}

type CostConfig struct {
	Endpoint string `toml:"endpoint" comment:"HTTP usage endpoint template for cost/token lookup. Use {session_id}; set empty to use agentsview CLI lookup only."`
	Timeout  string `toml:"timeout" comment:"Timeout for HTTP usage endpoint lookups."`
}

// ResolvedTimeout returns the HTTP usage lookup timeout.
func (c CostConfig) ResolvedTimeout() time.Duration {
	const defaultTimeout = 10 * time.Second
	if c.Timeout == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return defaultTimeout
	}
	return d
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

	// Cost/token usage lookup configuration
	Cost CostConfig `toml:"cost"`

	// Agent-specific behavior
	Agent AgentConfig `toml:"agent"`

	// Auto design review configuration (opt-in)
	AutoDesignReview AutoDesignReviewConfig `toml:"auto_design_review"`

	// Subagent review panels (opt-in)
	Review ReviewConfig `toml:"review"`

	// Optional agent harness hook integration
	AgentHook AgentHookConfig `toml:"agent_hook"`

	// Kata task-context integration for review prompts
	KataContext KataContextConfig `toml:"kata_context"`

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

	// Subagent review panel overrides for this repo (opt-in)
	Review ReviewConfig `toml:"review"`

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

	// Kata task-context integration for review prompts (per-repo)
	KataContext KataContextConfig `toml:"kata_context"`

	// Analysis settings
	MaxPromptSize int `toml:"max_prompt_size" comment:"Maximum prompt size for this repo before falling back to file paths."` // Max prompt size in bytes before falling back to paths (overrides global default)

	// ACP (Agent Client Protocol) configuration for this repo
	ACP *ACPAgentConfig `toml:"acp"`
}

const (
	DefaultPiJSONSchemaExtension = "npm:@nqbao/pi-json-schema@0.1.1"
)

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
		Cost: CostConfig{
			Timeout: "10s",
		},
		AgentHook: AgentHookConfig{
			TurnThreshold:         5,
			CommitThreshold:       0,
			FailedReviewThreshold: 4,
			Instruction:           "Invoke the $roborev-fix skill now.",
		},
		KataContext: KataContextConfig{Mode: KataModeOff, MaxChars: defaultKataMaxChars},
		Agent: AgentConfig{
			Codex: CodexConfig{
				DisableReviewSkills:    true,
				IgnoreReviewUserConfig: true,
			},
			Pi: PiConfig{
				JSONSchemaExtension: DefaultPiJSONSchemaExtension,
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

// RepoConfigPath returns the .roborev.toml path that should be read for
// repoPath, applying the linked-worktree fallback.
//
// A .roborev.toml that is gitignored (or otherwise untracked) lives only in
// the main checkout's working tree, so it is invisible from a worktree's
// directory. When repoPath has no .roborev.toml of its own, this resolves the
// main repository root via the git common dir and returns its config path.
//
// The fallback applies only when the main config is untracked. A tracked
// .roborev.toml is a versioned, branch-specific file: a worktree on a branch
// that removed it (or predates it) must not silently inherit the main
// checkout's branch copy, so in that case the worktree's own absence wins.
//
// Otherwise it returns repoPath's own (possibly nonexistent) config path, so
// callers keep their existing "file missing" behavior.
//
// All per-repo config loaders (decoded and raw) route through this so they
// agree on which file is authoritative.
func RepoConfigPath(repoPath string) string {
	local := filepath.Join(repoPath, ".roborev.toml")
	if _, err := os.Stat(local); err == nil {
		return local
	}

	// Not present (or unreadable) at repoPath. If repoPath is a worktree, the
	// config may live only in the main checkout. Any resolution failure (not a
	// git repo, etc.) falls through to the local path.
	mainRoot, err := git.GetMainRepoRoot(repoPath)
	if err != nil || mainRoot == "" || mainRoot == repoPath {
		return local
	}
	mainPath := filepath.Join(mainRoot, ".roborev.toml")
	if _, err := os.Stat(mainPath); err != nil {
		return local
	}
	// Only inherit an untracked (gitignored/machine-local) main config. A
	// tracked file belongs to the main checkout's branch, not this worktree.
	tracked, err := git.HasTrackedFilesUnder(mainRoot, mainPath)
	if err != nil || tracked {
		return local
	}
	return mainPath
}

// LoadRepoConfig loads per-repo config from .roborev.toml, applying the
// linked-worktree fallback via RepoConfigPath.
func LoadRepoConfig(repoPath string) (*RepoConfig, error) {
	path := RepoConfigPath(repoPath)
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

// ResolveAgentFromConfig is the config-taking core of ResolveAgent: it resolves
// entirely from the passed repoCfg and globalCfg, never reading the working tree.
func ResolveAgentFromConfig(explicit string, repoCfg *RepoConfig, globalCfg *Config) string {
	var repoVal string
	if repoCfg != nil {
		repoVal = repoCfg.Agent
	}
	var globalVal string
	if globalCfg != nil {
		globalVal = globalCfg.DefaultAgent
	}
	return resolve("codex", explicit, repoVal, globalVal)
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
	ctx context.Context,
	repoPath string, globalCfg *Config, reviewType string,
) []string {
	var repo []string
	if reviewType != "security" {
		repo = loadRepoExcludePatterns(ctx, repoPath)
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
func loadRepoExcludePatterns(ctx context.Context, repoPath string) []string {
	if defaultBranch, err := gitrepo.DefaultBranch(ctx, repoPath); err == nil {
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
const (
	DefaultMaxPromptSize = 200 * 1024
	DefaultSnapshotDir   = ".roborev"
)

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

// Kata context modes and the default cap on combined kata context bytes.
const (
	KataModeOff         = "off"
	KataModeCurrent     = "current"
	KataModeOpen        = "open"
	defaultKataMaxChars = 50000
)

// KataContextConfig controls pulling kata task context into review prompts.
type KataContextConfig struct {
	Mode     string `toml:"mode" comment:"Kata task context in review prompts: off | current | open (default off)."`
	MaxChars int    `toml:"max_chars" comment:"Max bytes of kata context to include (default 50000)."`
}

// ResolveKataContext returns the effective kata context settings for a repo,
// with per-repo values overriding the global config. Unknown modes resolve to
// "off"; a non-positive max_chars resolves to the default. The repo config is
// read from the working tree; callers that already hold a trusted repo config
// (e.g. CI, which loads it off the PR's default branch) should use
// ResolveKataContextFrom instead.
func ResolveKataContext(repoPath string, globalCfg *Config) KataContextConfig {
	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil {
		repoCfg = nil
	}
	return ResolveKataContextFrom(repoCfg, globalCfg)
}

// ResolveKataContextFrom is ResolveKataContext for an already-loaded repo
// config (nil means no repo-level overrides).
func ResolveKataContextFrom(repoCfg *RepoConfig, globalCfg *Config) KataContextConfig {
	mode := ""
	maxChars := 0
	if globalCfg != nil {
		mode = globalCfg.KataContext.Mode
		maxChars = globalCfg.KataContext.MaxChars
	}
	if repoCfg != nil {
		if repoCfg.KataContext.Mode != "" {
			mode = repoCfg.KataContext.Mode
		}
		if repoCfg.KataContext.MaxChars != 0 {
			maxChars = repoCfg.KataContext.MaxChars
		}
	}
	return KataContextConfig{Mode: normalizeKataMode(mode), MaxChars: clampKataMaxChars(maxChars)}
}

func normalizeKataMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case KataModeCurrent:
		return KataModeCurrent
	case KataModeOpen:
		return KataModeOpen
	default:
		return KataModeOff
	}
}

func clampKataMaxChars(n int) int {
	if n <= 0 {
		return defaultKataMaxChars
	}
	return n
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

// ResolveACPAgentConfigFromConfig is the config-taking core of
// ResolveACPAgentConfig. Repo-level ACP config completely overrides global ACP
// config when present.
func ResolveACPAgentConfigFromConfig(repoCfg *RepoConfig, globalCfg *Config) *ACPAgentConfig {
	if repoCfg != nil && repoCfg.ACP != nil {
		return repoCfg.ACP
	}
	if globalCfg != nil && globalCfg.ACP != nil {
		return globalCfg.ACP
	}
	return nil
}

// SaveGlobal saves the global configuration
func SaveGlobal(cfg *Config) error {
	return SaveGlobalTo(GlobalConfigPath(), cfg)
}

// SaveGlobalTo saves the global configuration to a specific path.
func SaveGlobalTo(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// SaveRepoConfigTo saves a per-repo configuration to a specific path.
func SaveRepoConfigTo(path string, cfg *RepoConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := tomlv2.Marshal(cfg)
	if err != nil {
		return err
	}

	mode := os.FileMode(0o644)
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
