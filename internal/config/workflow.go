// Review/workflow agent, model, reasoning, severity, and review-type resolution.

package config

import (
	"fmt"
	"reflect"
	"strings"
)

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
	return ResolveReviewReasoningFromConfig("", repoCfg, globalCfg)
}

// ResolveReviewReasoningFromConfig is the config-taking core of
// ResolveReviewReasoning: it resolves explicit > repoCfg > globalCfg > default
// ("thorough") entirely from the passed configs, never reading the working
// tree. When explicit is set it still validates the repo override field on the
// passed repoCfg before accepting the explicit value.
func ResolveReviewReasoningFromConfig(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if err := validateRepoReasoningOverrideValue(repoCfg, func(cfg *RepoConfig) string {
			return cfg.ReviewReasoning
		}); err != nil {
			return "", err
		}
		return NormalizeReasoning(explicit)
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
	return ResolveFixReasoningFromConfig("", repoCfg, globalCfg)
}

// ResolveFixReasoningFromConfig is the config-taking core of ResolveFixReasoning:
// it resolves explicit > repoCfg > globalCfg > default ("standard") entirely
// from the passed configs, never reading the working tree.
func ResolveFixReasoningFromConfig(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		if err := validateRepoReasoningOverrideValue(repoCfg, func(cfg *RepoConfig) string {
			return cfg.FixReasoning
		}); err != nil {
			return "", err
		}
		return NormalizeReasoning(explicit)
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
	if err != nil {
		return nil
	}

	return validateRepoReasoningOverrideValue(repoCfg, repoValue)
}

// validateRepoReasoningOverrideValue is the config-taking core of
// validateRepoReasoningOverride: it validates the repo's workflow reasoning
// override on the passed repoCfg (nil-safe) without reading the working tree.
// A nil repoCfg or an empty override is treated as valid; a non-empty override
// that fails NormalizeReasoning returns the normalization error.
func validateRepoReasoningOverrideValue(
	repoCfg *RepoConfig,
	repoValue func(*RepoConfig) string,
) error {
	if repoCfg == nil {
		return nil
	}
	reasoning := strings.TrimSpace(repoValue(repoCfg))
	if reasoning == "" {
		return nil
	}
	_, err := NormalizeReasoning(reasoning)
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
	repoCfg, _ := LoadRepoConfig(repoPath)
	return ResolveAgentForWorkflowFromConfig(cli, repoCfg, globalCfg, workflow, level)
}

// ResolveAgentForWorkflowFromConfig is the config-taking core of
// ResolveAgentForWorkflow: it resolves entirely from the passed repoCfg and
// globalCfg, never reading the working tree. Use this in contexts (e.g. CI
// panels) that must resolve from a config loaded off the default branch.
func ResolveAgentForWorkflowFromConfig(
	cli string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	workflow, level string,
) string {
	if s := strings.TrimSpace(cli); s != "" {
		return s
	}
	if s := getWorkflowValue(repoCfg, globalCfg, workflow, level, true); s != "" {
		return s
	}
	return "codex"
}

// HasWorkflowAgentOverrideFromConfig reports whether repo/global config has a
// workflow-specific primary agent override for the given workflow and reasoning
// level. It intentionally excludes generic repo agent and global default_agent
// values because those are preferences, not workflow pins.
func HasWorkflowAgentOverrideFromConfig(
	repoCfg *RepoConfig,
	globalCfg *Config,
	workflow, level string,
) bool {
	if repoCfg != nil {
		if repoWorkflowField(repoCfg, workflow, level, true) != "" ||
			repoWorkflowField(repoCfg, workflow, "", true) != "" {
			return true
		}
		if strings.TrimSpace(repoCfg.Agent) != "" {
			return false
		}
	}
	if globalCfg != nil {
		if globalWorkflowField(globalCfg, workflow, level, true) != "" ||
			globalWorkflowField(globalCfg, workflow, "", true) != "" {
			return true
		}
	}
	return false
}

// ResolveModelForWorkflow determines which model to use based on workflow and level.
// Same priority as ResolveAgentForWorkflow, but returns empty string as default.
func ResolveModelForWorkflow(cli, repoPath string, globalCfg *Config, workflow, level string) string {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return ResolveModelForWorkflowFromConfig(cli, repoCfg, globalCfg, workflow, level)
}

// ResolveModelForWorkflowFromConfig is the config-taking core of
// ResolveModelForWorkflow: it resolves entirely from the passed repoCfg and
// globalCfg, never reading the working tree.
func ResolveModelForWorkflowFromConfig(
	cli string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	workflow, level string,
) string {
	if s := strings.TrimSpace(cli); s != "" {
		return s
	}
	return getWorkflowValue(repoCfg, globalCfg, workflow, level, false)
}

// ResolveWorkflowModel resolves a model from workflow-specific config only,
// skipping generic defaults (repo model, global default_model). Use this
// when the agent was overridden from a different source (e.g., CLI --agent)
// and the generic model is likely paired with a different default agent.
func ResolveWorkflowModel(repoPath string, globalCfg *Config, workflow, level string) string {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return ResolveWorkflowModelFromConfig(repoCfg, globalCfg, workflow, level)
}

// ResolveWorkflowModelFromConfig is the config-taking core of
// ResolveWorkflowModel: it resolves a workflow-specific model from the passed
// repoCfg and globalCfg only, never reading the working tree.
func ResolveWorkflowModelFromConfig(
	repoCfg *RepoConfig,
	globalCfg *Config,
	workflow, level string,
) string {
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
	return ResolveBackupAgentForWorkflowFromConfig(repoCfg, globalCfg, workflow)
}

// ResolveBackupAgentForWorkflowFromConfig is the config-taking core of
// ResolveBackupAgentForWorkflow: it resolves entirely from the passed repoCfg
// and globalCfg, never reading the working tree.
func ResolveBackupAgentForWorkflowFromConfig(repoCfg *RepoConfig, globalCfg *Config, workflow string) string {
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
	return ResolveBackupModelForWorkflowFromConfig(repoCfg, globalCfg, workflow)
}

// ResolveBackupModelForWorkflowFromConfig is the config-taking core of
// ResolveBackupModelForWorkflow: it resolves entirely from the passed repoCfg
// and globalCfg, never reading the working tree.
func ResolveBackupModelForWorkflowFromConfig(repoCfg *RepoConfig, globalCfg *Config, workflow string) string {
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
