package daemon

import (
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// currentReviewTools resolves the tool policy a review job's type has now.
// Only code reviews carry a policy; goal reviews have their own rules.
func currentReviewTools(job storage.ReviewJob, repoCfg *config.RepoConfig, cfg *config.Config) (*config.ReviewToolPolicy, error) {
	if !job.IsReviewJob() || job.IsGoalReviewJob() || job.Source == storage.JobSourceRebuttal {
		return nil, nil
	}
	settings, err := config.ResolveReviewSettings(job.ReviewType, nil, repoCfg, cfg)
	return settings.Tools, err
}

// reviewJobTools returns the policy frozen in a job's member settings, so
// queued jobs and reruns keep the policy they were enqueued with. Jobs
// without frozen settings use their review type's current policy.
func reviewJobTools(job *storage.ReviewJob, configRepoPath string, cfg *config.Config) (*config.ReviewToolPolicy, error) {
	if !job.IsReviewJob() {
		return nil, nil
	}
	if job.PanelMemberConfigJSON != "" {
		var settings config.ReviewSettings
		if err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &settings); err != nil {
			return nil, fmt.Errorf("decode frozen review settings: %w", err)
		}
		return config.CanonicalReviewToolPolicy(settings.Tools)
	}
	if job.IsGoalReviewJob() || config.IsBuiltInReviewType(job.ReviewType) {
		return nil, nil
	}
	var repoCfg *config.RepoConfig
	var err error
	if job.IsCIReview() {
		var source ciRepoConfigSource
		source, err = loadCIRepoConfig(configRepoPath)
		repoCfg = source.Config
		if config.IsConfigParseError(err) {
			// Match CI prompt building, which reviews with global config.
			repoCfg, err = nil, nil
		}
	} else {
		repoCfg, err = config.LoadRepoConfig(configRepoPath)
	}
	if err != nil {
		return nil, fmt.Errorf("load review tool policy: %w", err)
	}
	return currentReviewTools(*job, repoCfg, cfg)
}

// validateMemberTools checks that each strict member's agent and backup can
// enforce its policy before any member job is enqueued.
func validateMemberTools(members []config.ResolvedMember, repoCfg *config.RepoConfig, cfg *config.Config) error {
	for _, m := range members {
		if m.Tools == nil {
			continue
		}
		selected, err := agent.GetPreferredOrBackupWithConfigFromConfig(repoCfg, m.Agent, cfg)
		if err == nil {
			err = agent.ValidateReviewToolSelection(m.Tools, selected, false)
		}
		if err == nil {
			err = agent.ValidateReviewToolBackup(m.Tools, agent.WorkflowConfig{
				RepoConfig: repoCfg, GlobalConfig: cfg, BackupAgent: m.BackupAgent,
			}, selected.Name())
		}
		if err != nil {
			return fmt.Errorf("panel member %q: %w", m.Name, err)
		}
	}
	return nil
}
