package daemon

import (
	"encoding/json"
	"fmt"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type ciPanelMemberConfig struct {
	config.ResolvedMember
	ACP config.ACPAgentConfigs `json:"acp,omitempty"`
}

type ciACPExecutionConfig struct {
	ACP config.ACPAgentConfigs `json:"acp,omitempty"`
}

func snapshotACPExecutionConfig(
	repoCfg *config.RepoConfig,
	cfg *config.Config,
	names ...string,
) config.ACPAgentConfigs {
	effective := config.ResolveACPAgentConfigsFromConfig(repoCfg, cfg)
	var snapshot config.ACPAgentConfigs
	for _, name := range names {
		name = agent.ACPConfigName(name)
		acpCfg, ok := effective[name]
		if name == "" || !ok {
			continue
		}
		if snapshot == nil {
			snapshot = make(config.ACPAgentConfigs)
		}
		snapshot[name] = acpCfg
	}
	return snapshot
}

func ciACPRepoConfig(job *storage.ReviewJob) (*config.RepoConfig, bool, error) {
	if job == nil || !job.IsCIReview() || job.PanelMemberConfigJSON == "" {
		return nil, false, nil
	}
	var snapshot ciACPExecutionConfig
	if err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &snapshot); err != nil {
		return nil, false, fmt.Errorf("decode CI ACP execution config: %w", err)
	}
	if len(snapshot.ACP) == 0 {
		return nil, false, nil
	}
	return &config.RepoConfig{ACP: snapshot.ACP}, true, nil
}

func resolveConfiguredJobAgent(
	job *storage.ReviewJob,
	cfg *config.Config,
	backups ...string,
) (agent.Agent, error) {
	repoCfg, snapshotted, err := ciACPRepoConfig(job)
	if err != nil {
		return nil, err
	}
	if snapshotted {
		return agent.GetPreferredOrBackupWithConfigFromConfig(
			repoCfg, job.Agent, cfg, backups...,
		)
	}
	return agent.GetPreferredOrBackupWithConfig(
		job.RepoPath, job.Agent, cfg, backups...,
	)
}

func resolveReviewJobAgent(job *storage.ReviewJob, cfg *config.Config) (agent.Agent, error) {
	return resolveConfiguredJobAgent(job, cfg)
}
