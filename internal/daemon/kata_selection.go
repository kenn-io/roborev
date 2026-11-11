package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/storage"
)

type kataSelection struct {
	seed    storage.KataPublicationSeed
	options kata.PublicationOptions
}

// selectKataPublication is deliberately offline: it never reads credentials.
func selectKataPublication(ctx context.Context, cfg *config.Config, job *storage.ReviewJob, checkout string) (*kataSelection, error) {
	if cfg == nil || !cfg.Integrations.Kata.Enabled || job.IsCIReview() || job.PanelRole == storage.PanelRoleMember || job.Source == storage.JobSourceRebuttal {
		return nil, nil
	}
	switch job.JobType {
	case storage.JobTypeReview, storage.JobTypeRange, storage.JobTypeDirty, storage.JobTypeSynthesis:
	default:
		return nil, nil
	}
	hooks := append([]config.HookConfig(nil), cfg.Hooks...)
	if checkout != "" && !hasSuppliedReviewSettings(job) {
		rc, err := config.LoadRepoConfig(checkout)
		if err != nil {
			return nil, err
		}
		if rc != nil {
			hooks = append(hooks, rc.Hooks...)
		}
	}
	endpoint, err := kata.NormalizePublicationEndpoint(cfg.Integrations.Kata.Endpoint)
	if err != nil {
		return nil, err
	}
	var selected *kataSelection
	for _, hook := range hooks {
		if hook.Type != "kata" || hook.Findings == "" || !matchEvent(hook.Event, "review.completed") || !matchBranch(hook.Branches, job.HookBranch()) {
			continue
		}
		if hook.Findings != "comments" && hook.Findings != "summary" {
			return nil, errors.New("invalid Kata findings policy")
		}
		candidate := kataSelection{seed: storage.KataPublicationSeed{Endpoint: endpoint, Project: strings.TrimSpace(hook.Project), Policy: hook.Findings}, options: kata.PublicationOptions{Labels: append([]string(nil), hook.Labels...), Priority: hook.Priority}}
		slices.Sort(candidate.options.Labels)
		candidate.options.Labels = slices.Compact(candidate.options.Labels)
		if candidate.seed.Project == "" {
			binding, bindErr := kata.WorkspaceBinding(ctx, checkout)
			if errors.Is(bindErr, kata.ErrNoBinding) {
				candidate.seed.Project = strings.TrimSpace(cfg.Integrations.Kata.DefaultProject)
			} else if bindErr != nil {
				return nil, bindErr
			} else {
				candidate.seed.Project = binding.Project
				if binding.Alias != nil {
					candidate.seed.AliasKind, candidate.seed.AliasIdentity = binding.Alias.Kind, binding.Alias.Identity
				}
			}
		}
		if candidate.seed.Project == "" && candidate.seed.AliasKind == "" {
			return nil, errors.New("no Kata project selected")
		}
		if selected != nil && (selected.seed != candidate.seed || !slices.Equal(selected.options.Labels, candidate.options.Labels) || !sameKataPriority(selected.options.Priority, candidate.options.Priority)) {
			return nil, errors.New("conflicting Kata publication hooks")
		}
		selected = &candidate
	}
	return selected, nil
}

func sameKataPriority(a, b *int) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (wp *WorkerPool) completeReviewResult(ctx context.Context, workerID string, job *storage.ReviewJob, checkout, agent string, completion storage.ReviewCompletion) error {
	selection, err := selectKataPublication(ctx, wp.cfgGetter.Config(), job, checkout)
	if err != nil {
		log.Printf("[%s] Kata publication authorization for job %d: %v", workerID, job.ID, err)
	} else if selection != nil {
		completion.KataPublication = &selection.seed
	}
	if err := wp.db.CompleteJobResult(job.ID, agent, completion); err != nil {
		return fmt.Errorf("complete review: %w", err)
	}
	return nil
}
