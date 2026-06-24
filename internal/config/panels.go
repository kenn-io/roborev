// Subagent review panel configuration and resolution.

package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// SubagentSpec is a named reviewer spec referenced by panels. Empty scalar
// fields fall back to the existing workflow resolution at resolve time, so a
// subagent may omit model/reasoning and inherit the workflow default.
type SubagentSpec struct {
	Agent        string `toml:"agent"`
	Model        string `toml:"model"`
	Provider     string `toml:"provider"`
	Reasoning    string `toml:"reasoning"`
	ReviewType   string `toml:"review_type"`
	Instructions string `toml:"instructions"`
	AllowFailure bool   `toml:"allow_failure"`
	Timeout      string `toml:"timeout"`
}

// PanelSpec is a named set of subagent members plus an optional synthesis
// agent/model. Synthesis falls back to the fix-workflow resolution when unset.
// The synthesis backup agent/model are an explicit opt-in: they pass straight
// through to the synthesis job's stored failover backup, with no resolution or
// fallback.
type PanelSpec struct {
	Members              []string `toml:"members"`
	SynthesisAgent       string   `toml:"synthesis_agent"`
	SynthesisModel       string   `toml:"synthesis_model"`
	SynthesisBackupAgent string   `toml:"synthesis_backup_agent"`
	SynthesisBackupModel string   `toml:"synthesis_backup_model"`
}

// ReviewConfig holds the [review] table: panel selection defaults plus the
// named subagent and panel maps. Present on both Config (global) and
// RepoConfig (per-repo); MergeReviewConfig combines them.
type ReviewConfig struct {
	DefaultPanel string                  `toml:"default_panel"`
	HookPanel    string                  `toml:"hook_review_panel"`
	Subagents    map[string]SubagentSpec `toml:"subagents"`
	Panels       map[string]PanelSpec    `toml:"panels"`
}

// MergeReviewConfig returns the effective review config: the subagent and
// panel maps are the union of global and repo with repo keys overriding global
// keys; DefaultPanel and HookPanel are repo-over-global. Inputs are not mutated.
func MergeReviewConfig(repo, global ReviewConfig) ReviewConfig {
	merged := ReviewConfig{
		DefaultPanel: resolve("", repo.DefaultPanel, global.DefaultPanel),
		HookPanel:    resolve("", repo.HookPanel, global.HookPanel),
		Subagents:    make(map[string]SubagentSpec, len(global.Subagents)+len(repo.Subagents)),
		Panels:       make(map[string]PanelSpec, len(global.Panels)+len(repo.Panels)),
	}
	maps.Copy(merged.Subagents, global.Subagents)
	maps.Copy(merged.Subagents, repo.Subagents)
	maps.Copy(merged.Panels, global.Panels)
	maps.Copy(merged.Panels, repo.Panels)
	return merged
}

// MergedReviewConfig loads the repo's review config (if any) and merges it over
// the global review config. When repoPath is empty or whitespace, no repo
// config is loaded (global only) — guarding against LoadRepoConfig resolving
// ".roborev.toml" relative to the process working directory.
func MergedReviewConfig(repoPath string, globalCfg *Config) ReviewConfig {
	var repo ReviewConfig
	if strings.TrimSpace(repoPath) != "" {
		if repoCfg, err := LoadRepoConfig(repoPath); err == nil && repoCfg != nil {
			repo = repoCfg.Review
		}
	}
	var global ReviewConfig
	if globalCfg != nil {
		global = globalCfg.Review
	}
	return MergeReviewConfig(repo, global)
}

// Validate reports every cross-reference problem in the review config: panels
// with no members, panel members that name an undefined subagent, and
// DefaultPanel/HookPanel that name an undefined panel. It aggregates all
// problems into one error (deterministic, panel-name-sorted order) rather than
// failing on the first, and returns nil when clean.
func (rc ReviewConfig) Validate() error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(rc.Panels)) {
		panel := rc.Panels[name]
		if len(panel.Members) == 0 {
			errs = append(errs, fmt.Errorf("panel %q has no members", name))
			continue
		}
		for _, member := range panel.Members {
			if _, ok := rc.Subagents[member]; !ok {
				errs = append(errs, fmt.Errorf("panel %q references undefined subagent %q", name, member))
			}
		}
	}
	errs = append(errs, rc.checkPanelRef("default_panel", rc.DefaultPanel))
	errs = append(errs, rc.checkPanelRef("hook_review_panel", rc.HookPanel))
	return errors.Join(errs...)
}

// checkPanelRef returns an error if name is non-empty but is not a defined
// panel, else nil. key is the config key (e.g. "default_panel") named in the
// message so the user sees the TOML field they wrote.
func (rc ReviewConfig) checkPanelRef(key, name string) error {
	if name == "" {
		return nil
	}
	if _, ok := rc.Panels[name]; !ok {
		return fmt.Errorf("%s %q is not a defined panel", key, name)
	}
	return nil
}

// PanelNone is the reserved --panel value that forces a single-agent review,
// overriding any default_panel/hook_review_panel in any context.
const PanelNone = "none"

// SelectPanelName routes a panel request by provenance and returns the panel
// name to run; "" means a single-agent review (no panel). Precedence, highest
// first:
//
//  1. requested == PanelNone ("none") -> "" (force single-agent in any context).
//  2. any other non-empty requested -> requested (explicit --panel wins).
//  3. source == "" (foreground) -> merged.DefaultPanel.
//  4. source != "" (automatic, e.g. "post_commit") -> merged.HookPanel
//     (DefaultPanel is never consulted).
//
// A returned non-empty name is not guaranteed to be a defined panel; ResolvePanel
// reports an unknown name as a hard error.
func SelectPanelName(requested, source string, merged ReviewConfig) string {
	// Cases are precedence-ordered; PanelNone must precede the generic non-empty
	// case so "none" forces single-agent instead of passing through as a name.
	switch {
	case requested == PanelNone:
		return ""
	case requested != "":
		return requested
	case source == "":
		return merged.DefaultPanel
	default:
		return merged.HookPanel
	}
}

// ResolvedMember is a fully-resolved panel member. It is the value serialized
// into the review_jobs.panel_member_config_json column for reproducibility.
type ResolvedMember struct {
	Name          string `json:"name"`
	Index         int    `json:"index"`
	Agent         string `json:"agent"`
	AgentExplicit bool   `json:"agent_explicit,omitempty"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	Reasoning     string `json:"reasoning"`
	ReviewType    string `json:"review_type"`
	Instructions  string `json:"instructions"`
	AllowFailure  bool   `json:"allow_failure,omitempty"`
	Timeout       string `json:"timeout,omitempty"`
}

// SynthesisSpec is the resolved agent/model/reasoning for a panel's synthesis
// job. BackupAgent/BackupModel are the explicit synthesis failover backup,
// passed through verbatim from the panel spec (no resolution or fallback).
type SynthesisSpec struct {
	Agent       string `json:"agent"`
	Model       string `json:"model"`
	Reasoning   string `json:"reasoning"`
	BackupAgent string `json:"backup_agent"`
	BackupModel string `json:"backup_model"`
}

// ResolvePanel resolves a named panel into ordered members and a synthesis
// spec, merging the repo and global review config. It is a hard error if the
// panel is undefined, has no members, references an undefined subagent, or a
// member has an invalid review_type/reasoning.
func ResolvePanel(panelName, repoPath string, globalCfg *Config) ([]ResolvedMember, SynthesisSpec, error) {
	merged := MergedReviewConfig(repoPath, globalCfg)
	panel, ok := merged.Panels[panelName]
	if !ok {
		return nil, SynthesisSpec{}, fmt.Errorf("panel %q is not defined", panelName)
	}
	if len(panel.Members) == 0 {
		return nil, SynthesisSpec{}, fmt.Errorf("panel %q has no members", panelName)
	}
	members := make([]ResolvedMember, 0, len(panel.Members))
	for i, name := range panel.Members {
		spec, ok := merged.Subagents[name]
		if !ok {
			return nil, SynthesisSpec{}, fmt.Errorf("panel %q references undefined subagent %q", panelName, name)
		}
		member, err := resolveMember(name, i, spec, repoPath, globalCfg)
		if err != nil {
			return nil, SynthesisSpec{}, err
		}
		members = append(members, member)
	}
	synth, err := resolveSynthesis(panel, repoPath, globalCfg)
	if err != nil {
		return nil, SynthesisSpec{}, err
	}
	return members, synth, nil
}

// ResolveCIPanel resolves a named panel for CI from the passed configs only. It
// is the F1-safe counterpart to ResolvePanel: repoCfg is the config loaded off
// the PR's default branch (never the daemon's working tree), so this function
// and every helper it calls resolve entirely from repoCfg + globalCfg and never
// call LoadRepoConfig or otherwise read the working tree. A conflicting
// .roborev.toml in the process working directory is ignored. Error semantics
// (undefined panel, no members, undefined subagent, invalid review_type/
// reasoning) match ResolvePanel.
func ResolveCIPanel(
	panelName string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) ([]ResolvedMember, SynthesisSpec, error) {
	var repoReview ReviewConfig
	if repoCfg != nil {
		repoReview = repoCfg.Review
	}
	var global ReviewConfig
	if globalCfg != nil {
		global = globalCfg.Review
	}
	merged := MergeReviewConfig(repoReview, global)

	panel, ok := merged.Panels[panelName]
	if !ok {
		return nil, SynthesisSpec{}, fmt.Errorf("panel %q is not defined", panelName)
	}
	if len(panel.Members) == 0 {
		return nil, SynthesisSpec{}, fmt.Errorf("panel %q has no members", panelName)
	}
	members := make([]ResolvedMember, 0, len(panel.Members))
	for i, name := range panel.Members {
		spec, ok := merged.Subagents[name]
		if !ok {
			return nil, SynthesisSpec{}, fmt.Errorf("panel %q references undefined subagent %q", panelName, name)
		}
		member, err := resolveMemberFromConfig(name, i, spec, repoCfg, globalCfg)
		if err != nil {
			return nil, SynthesisSpec{}, err
		}
		members = append(members, member)
	}
	synth, err := resolveSynthesisFromConfig(panel, repoCfg, globalCfg)
	if err != nil {
		return nil, SynthesisSpec{}, err
	}
	return members, synth, nil
}

// ResolveCISynthesis resolves the synthesis spec for the implicit-panel
// (agents x review_types matrix) CI path, where no named [review.panels.X]
// supplies synthesis settings. It resolves the synthesis agent/model from the
// [ci] synthesis config when present, otherwise from the fix workflow off the
// passed default-branch config (F1), then overrides the reasoning to the supplied
// CI reasoning so synthesis runs at the same level as the member reviews.
func ResolveCISynthesis(
	ciReasoning string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (SynthesisSpec, error) {
	panel := PanelSpec{}
	if globalCfg != nil {
		panel.SynthesisAgent = globalCfg.CI.SynthesisAgent
		panel.SynthesisModel = globalCfg.CI.SynthesisModel
		panel.SynthesisBackupAgent = globalCfg.CI.SynthesisBackupAgent
	}
	synth, err := resolveSynthesisFromConfig(panel, repoCfg, globalCfg)
	if err != nil {
		return SynthesisSpec{}, err
	}
	synth.Reasoning = ciReasoning
	return synth, nil
}

// resolveMember fills a member's empty scalar fields from the workflow
// resolution that matches its review type, canonicalizing review_type. An
// omitted reasoning inherits the review default (every member, including
// security/design ones, follows the review reasoning, not its own workflow's).
// An omitted model on a member that pins an explicit agent inherits only a
// workflow-specific model, never a generic default_model/repo model paired with
// a different default agent.
func resolveMember(name string, index int, spec SubagentSpec, repoPath string, globalCfg *Config) (ResolvedMember, error) {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return resolveMemberFromConfig(name, index, spec, repoCfg, globalCfg)
}

// resolveMemberFromConfig is the config-taking core of resolveMember: it fills
// the member's empty scalar fields from the passed repoCfg and globalCfg only,
// never reading the working tree (F1). Resolution semantics are identical to
// resolveMember.
func resolveMemberFromConfig(
	name string,
	index int,
	spec SubagentSpec,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (ResolvedMember, error) {
	reviewType, err := canonicalMemberReviewType(spec.ReviewType)
	if err != nil {
		return ResolvedMember{}, fmt.Errorf("subagent %q: %w", name, err)
	}
	reasoning, err := ResolveReviewReasoningFromConfig(spec.Reasoning, repoCfg, globalCfg)
	if err != nil {
		return ResolvedMember{}, fmt.Errorf("subagent %q: %w", name, err)
	}
	if err := validateSubagentTimeout(spec.Timeout); err != nil {
		return ResolvedMember{}, fmt.Errorf("subagent %q: %w", name, err)
	}
	workflow := workflowForReviewType(reviewType)
	agent := spec.Agent
	if agent == "" {
		agent = ResolveAgentForWorkflowFromConfig("", repoCfg, globalCfg, workflow, reasoning)
	}
	model := spec.Model
	if model == "" {
		if spec.Agent != "" {
			// Explicit agent: inherit only a workflow-specific model; never a
			// generic default_model/repo model paired with a different default
			// agent.
			model = ResolveWorkflowModelFromConfig(repoCfg, globalCfg, workflow, reasoning)
		} else {
			model = ResolveModelForWorkflowFromConfig("", repoCfg, globalCfg, workflow, reasoning)
		}
	}
	return ResolvedMember{
		Name:          name,
		Index:         index,
		Agent:         agent,
		AgentExplicit: strings.TrimSpace(spec.Agent) != "",
		Model:         model,
		Provider:      spec.Provider,
		Reasoning:     reasoning,
		ReviewType:    reviewType,
		Instructions:  spec.Instructions,
		AllowFailure:  spec.AllowFailure,
		Timeout:       spec.Timeout,
	}, nil
}

func validateSubagentTimeout(timeout string) error {
	if timeout == "" {
		return nil
	}
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		return fmt.Errorf("invalid timeout %q", timeout)
	}
	return nil
}

// resolveSynthesis resolves the synthesis agent/model/reasoning: the panel's
// explicit synthesis_agent/synthesis_model else the fix-workflow resolution,
// and the fix-workflow reasoning (synthesis consolidates like a fix). An
// omitted synthesis_model on a panel that pins an explicit synthesis_agent
// inherits only a workflow-specific fix model, never a generic default_model/
// repo model paired with a different default agent (mirrors member resolution).
func resolveSynthesis(panel PanelSpec, repoPath string, globalCfg *Config) (SynthesisSpec, error) {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return resolveSynthesisFromConfig(panel, repoCfg, globalCfg)
}

// resolveSynthesisFromConfig is the config-taking core of resolveSynthesis: it
// resolves the synthesis agent/model/reasoning from the passed repoCfg and
// globalCfg only, never reading the working tree (F1). Resolution semantics are
// identical to resolveSynthesis.
func resolveSynthesisFromConfig(
	panel PanelSpec,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (SynthesisSpec, error) {
	reasoning, err := ResolveFixReasoningFromConfig("", repoCfg, globalCfg)
	if err != nil {
		return SynthesisSpec{}, err
	}
	agent := panel.SynthesisAgent
	if agent == "" {
		agent = ResolveAgentForWorkflowFromConfig("", repoCfg, globalCfg, "fix", reasoning)
	}
	model := panel.SynthesisModel
	if model == "" {
		if panel.SynthesisAgent != "" {
			// Explicit synthesis agent: inherit only a workflow-specific fix
			// model, never a generic default_model/repo model paired with a
			// different default agent.
			model = ResolveWorkflowModelFromConfig(repoCfg, globalCfg, "fix", reasoning)
		} else {
			model = ResolveModelForWorkflowFromConfig("", repoCfg, globalCfg, "fix", reasoning)
		}
	}
	return SynthesisSpec{
		Agent: agent, Model: model, Reasoning: reasoning,
		BackupAgent: panel.SynthesisBackupAgent,
		BackupModel: panel.SynthesisBackupModel,
	}, nil
}

// workflowForReviewType maps a canonical review type to the workflow name used
// for agent/model fallback resolution.
func workflowForReviewType(reviewType string) string {
	switch reviewType {
	case ReviewTypeSecurity:
		return "security"
	case ReviewTypeDesign:
		return "design"
	default:
		return "review"
	}
}

// canonicalMemberReviewType canonicalizes a subagent's review_type, treating
// empty as "default".
func canonicalMemberReviewType(reviewType string) (string, error) {
	if reviewType == "" {
		return ReviewTypeDefault, nil
	}
	canonical, err := ValidateReviewTypes([]string{reviewType})
	if err != nil {
		return "", err
	}
	return canonical[0], nil
}
