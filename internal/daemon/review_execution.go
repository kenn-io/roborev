package daemon

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// reviewExecutionSnapshot is the complete existing member/CI envelope, not a
// reduced policy schema. Keep the original JSON on the job for rerun copying.
type reviewExecutionSnapshot struct {
	config.ResolvedMember
	ACP     config.ACPAgentConfigs `json:"acp,omitempty"`
	timeout time.Duration
}

// reviewJobConfigPath keeps local execution and rerun configuration tied to
// the recorded checkout. Goal reviews retain their source-specific rules.
func reviewJobConfigPath(job *storage.ReviewJob) (string, error) {
	if job.IsGoalReviewJob() {
		return GoalReviewConfigRepoPath(job), nil
	}
	if job.WorktreePath == "" {
		return job.RepoPath, nil
	}
	if path := validatedWorktreePath(job.WorktreePath, job.RepoPath); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("job worktree path is stale or invalid")
}

func resolveReviewJobSettings(job *storage.ReviewJob, snapshot reviewExecutionSnapshot, repoPath string, cfg *config.Config) (config.ReviewSettings, error) {
	if !job.IsReviewJob() || job.IsGoalReviewJob() {
		return config.ReviewSettings{}, nil
	}
	if job.PanelMemberConfigJSON != "" {
		return snapshot.ReviewSettings, nil
	}
	var repoCfg *config.RepoConfig
	var err error
	if job.IsCIReview() {
		var source config.RepoConfigSource
		source, err = loadCIRepoConfig(repoPath)
		repoCfg = source.Config
	} else {
		repoCfg, err = config.LoadRepoConfig(repoPath)
	}
	if err != nil {
		return config.ReviewSettings{}, err
	}
	return config.ResolveReviewSettings(job.ReviewType, nil, repoCfg, cfg)
}

func decodeReviewExecutionSnapshot(raw string) (reviewExecutionSnapshot, error) {
	var snapshot reviewExecutionSnapshot
	if raw == "" {
		return snapshot, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return snapshot, fmt.Errorf("review execution snapshot must be an object")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return snapshot, fmt.Errorf("decode review execution snapshot: %w", err)
	}
	// json/v2 accepts null for scalar zero values; present execution fields
	// must carry their declared type instead of silently weakening policy.
	for _, key := range []string{"tools", "binary_evidence", "evidence_manifest", "instructions", "timeout", "acp", "name", "index", "agent", "model", "provider", "reasoning", "review_type", "allow_failure", "agent_explicit", "model_explicit", "backup_agent", "backup_model"} {
		if string(fields[key]) == "null" {
			return snapshot, fmt.Errorf("review execution field %q cannot be null", key)
		}
	}
	if tools, present := fields["tools"]; present {
		var policyFields map[string]jsontext.Value
		if err := json.Unmarshal(tools, &policyFields); err != nil {
			return snapshot, fmt.Errorf("decode review tools: %w", err)
		}
		if string(policyFields["mcp"]) == "null" {
			return snapshot, fmt.Errorf("review tools mcp cannot be null")
		}
		if mcp, present := policyFields["mcp"]; present {
			var grants map[string]jsontext.Value
			if err := json.Unmarshal(mcp, &grants); err != nil {
				return snapshot, fmt.Errorf("decode review tools mcp: %w", err)
			}
			for server, tools := range grants {
				if tools.Kind() != jsontext.KindBeginArray {
					return snapshot, fmt.Errorf("review tools mcp grants for %q must be an array", server)
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return snapshot, fmt.Errorf("decode review execution snapshot: %w", err)
	}
	var err error
	snapshot.Tools, err = config.CanonicalReviewToolPolicy(snapshot.Tools)
	if err != nil {
		return reviewExecutionSnapshot{}, err
	}
	if snapshot.Timeout != "" {
		snapshot.timeout, err = time.ParseDuration(snapshot.Timeout)
		if err != nil || snapshot.timeout <= 0 {
			return reviewExecutionSnapshot{}, fmt.Errorf("invalid review execution timeout %q", snapshot.Timeout)
		}
	}
	for name, acp := range snapshot.ACP {
		if err := config.ValidateACPAgentConfig(name, acp); err != nil {
			return reviewExecutionSnapshot{}, err
		}
	}
	return snapshot, nil
}

// validateReviewExecutionPolicy validates frozen values against the final
// requested adapter and its configured backup. Operator authority stays live.
func validateReviewExecutionPolicy(job *storage.ReviewJob, cfg *config.Config, selectedAgent, backup string) error {
	snapshot, err := decodeReviewExecutionSnapshot(job.PanelMemberConfigJSON)
	if err != nil {
		return err
	}
	if job.IsGoalReviewJob() {
		return nil
	}
	configPath, err := reviewJobConfigPath(job)
	if err != nil {
		return err
	}
	settings := snapshot.ReviewSettings
	if job.PanelMemberConfigJSON == "" && !job.IsSynthesisJob() && !config.IsBuiltInReviewType(job.ReviewType) {
		settings, err = resolveReviewJobSettings(job, snapshot, configPath, cfg)
		if err != nil {
			return err
		}
	}
	if settings.Tools == nil {
		return nil
	}
	target := *job
	if selectedAgent != "" {
		target.Agent = selectedAgent
	}
	selected, err := resolveConfiguredJobAgentWithSnapshot(&target, cfg, snapshot, backup)
	if err != nil {
		return err
	}
	if err := agent.ValidateReviewToolSelection(settings.Tools, selected, job.Agentic); err != nil {
		return err
	}
	return agent.ValidateReviewToolBackup(settings.Tools, agent.WorkflowConfig{GlobalConfig: cfg, RepoConfig: &config.RepoConfig{ACP: snapshot.ACP}, BackupAgent: backup}, selected.Name())
}

func synthesisExecutionSnapshot(members []config.ResolvedMember, acp config.ACPAgentConfigs) reviewExecutionSnapshot {
	snapshot := reviewExecutionSnapshot{ACP: acp}
	for _, member := range members {
		if !member.NonVoting && member.Tools != nil {
			snapshot.Tools = &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}
			break
		}
	}
	return snapshot
}
