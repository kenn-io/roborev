package daemon

import (
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// currentReviewSettings resolves the settings a review job's type has now.
// Only code reviews carry settings; goal reviews have their own rules.
func currentReviewSettings(job storage.ReviewJob, repoCfg *config.RepoConfig, cfg *config.Config) (config.ReviewSettings, error) {
	if !job.IsReviewJob() || job.IsGoalReviewJob() || job.Source == storage.JobSourceRebuttal {
		return config.ReviewSettings{}, nil
	}
	return config.ResolveReviewSettings(job.ReviewType, nil, repoCfg, cfg)
}

// currentReviewTools resolves the tool policy a review job's type has now.
func currentReviewTools(job storage.ReviewJob, repoCfg *config.RepoConfig, cfg *config.Config) (*config.ReviewToolPolicy, error) {
	settings, err := currentReviewSettings(job, repoCfg, cfg)
	return settings.Tools, err
}

// reviewJobSettings returns the settings frozen in a job's member config, so
// queued jobs and reruns keep the settings they were enqueued with. Jobs
// without frozen settings use their review type's current settings.
func reviewJobSettings(job *storage.ReviewJob, configRepoPath string, cfg *config.Config) (config.ReviewSettings, error) {
	var settings config.ReviewSettings
	if !job.IsReviewJob() {
		return settings, nil
	}
	if job.PanelMemberConfigJSON != "" {
		if err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &settings); err != nil {
			return settings, fmt.Errorf("decode frozen review settings: %w", err)
		}
		var err error
		settings.Tools, err = config.CanonicalReviewToolPolicy(settings.Tools)
		return settings, err
	}
	if job.IsGoalReviewJob() || config.IsBuiltInReviewType(job.ReviewType) {
		return settings, nil
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
		return settings, fmt.Errorf("load review settings: %w", err)
	}
	return currentReviewSettings(*job, repoCfg, cfg)
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
