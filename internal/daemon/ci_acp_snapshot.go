package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type ciPanelMemberConfig = reviewExecutionSnapshot

type ciACPExecutionConfig struct {
	ACP config.ACPAgentConfigs `json:"acp,omitempty"`
}

func snapshotACPExecutionConfig(
	repoCfg *config.RepoConfig,
	cfg *config.Config,
	names ...string,
) (config.ACPAgentConfigs, error) {
	effective := config.ResolveACPAgentConfigsFromConfig(repoCfg, cfg)
	var snapshot config.ACPAgentConfigs
	for _, name := range names {
		canonicalName := strings.TrimSpace(name)
		configName := agent.ACPConfigName(canonicalName)
		if configName == "" {
			continue
		}
		acpCfg, ok := effective[configName]
		if !ok {
			return nil, fmt.Errorf("ACP agent %q is not configured", canonicalName)
		}
		if snapshot == nil {
			snapshot = make(config.ACPAgentConfigs)
		}
		snapshot[configName] = acpCfg
	}
	return snapshot, nil
}

func resolveConfiguredJobAgent(
	job *storage.ReviewJob,
	cfg *config.Config,
	backups ...string,
) (agent.Agent, error) {
	snapshot, err := decodeReviewExecutionSnapshot(job.PanelMemberConfigJSON)
	if err != nil {
		return nil, err
	}
	return resolveConfiguredJobAgentWithSnapshot(job, cfg, snapshot, backups...)
}

func resolveConfiguredJobAgentWithSnapshot(job *storage.ReviewJob, cfg *config.Config, snapshot reviewExecutionSnapshot, backups ...string) (agent.Agent, error) {
	if len(snapshot.ACP) > 0 || job.IsCIReview() && job.PanelMemberConfigJSON != "" {
		acp, err := localPanelACPWithDynamicBackup(job, cfg, snapshot, append([]string{job.Agent}, backups...))
		if err != nil {
			return nil, err
		}
		return agent.GetPreferredOrBackupWithConfigFromConfig(
			&config.RepoConfig{ACP: acp}, job.Agent, cfg, backups...,
		)
	}
	if job.IsCIReview() {
		source, err := loadCIRepoConfig(job.RepoPath)
		if err != nil {
			return nil, err
		}
		return agent.GetPreferredOrBackupWithConfigFromConfig(source.Config, job.Agent, cfg, backups...)
	}
	path, err := reviewJobConfigPath(job)
	if err != nil {
		return nil, err
	}
	return agent.GetPreferredOrBackupWithConfig(path, job.Agent, cfg, backups...)
}

func resolveReviewJobAgent(job *storage.ReviewJob, cfg *config.Config) (agent.Agent, error) {
	return resolveConfiguredJobAgent(job, cfg)
}

// Ordinary local panels may acquire a workflow backup after enqueue. Resolve
// only that missing definition live; every entry already frozen stays frozen.
func localPanelACPWithDynamicBackup(job *storage.ReviewJob, cfg *config.Config, snapshot reviewExecutionSnapshot, requested []string) (config.ACPAgentConfigs, error) {
	if job.IsCIReview() || job.PanelRole != storage.PanelRoleMember ||
		job.FrozenExperimentPlan != nil || len(job.Experiments) != 0 ||
		job.BackupAgent != "" || snapshot.BackupAgent != "" {
		return snapshot.ACP, nil
	}
	needsLive := slices.ContainsFunc(requested, func(name string) bool {
		key := agent.ACPConfigName(name)
		_, frozen := snapshot.ACP[key]
		return key != "" && !frozen
	})
	if !needsLive {
		return snapshot.ACP, nil
	}
	path, err := reviewJobConfigPath(job)
	if err != nil {
		return nil, err
	}
	repoCfg, err := config.LoadRepoConfig(path)
	if err != nil {
		return nil, err
	}
	backup := config.ResolveBackupAgentForWorkflowFromConfig(repoCfg, cfg, failoverWorkflow(job))
	key := agent.ACPConfigName(backup)
	if key == "" || !slices.Contains(requested, backup) {
		return snapshot.ACP, nil
	}
	if _, frozen := snapshot.ACP[key]; frozen {
		return snapshot.ACP, nil
	}
	entry, ok := config.ResolveACPAgentConfigFromConfig(key, repoCfg, cfg)
	if !ok {
		return snapshot.ACP, nil
	}
	acp := maps.Clone(snapshot.ACP)
	acp[key] = entry
	return acp, nil
}
