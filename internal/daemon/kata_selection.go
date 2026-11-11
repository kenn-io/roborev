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
	seed        storage.KataPublicationSeed
	options     kata.PublicationOptions
	resolvedUID string // populated only when different selections required comparison
}

// Single or identical selections stay offline; distinct selections need a
// server identity check before they can authorize one destination.
func selectKataPublication(ctx context.Context, cfg *config.Config, job *storage.ReviewJob, checkout string) (*kataSelection, error) {
	if cfg == nil || !cfg.Integrations.Kata.Enabled || job.IsCIReview() || job.PanelRole == storage.PanelRoleMember || job.Source == storage.JobSourceRebuttal {
		return nil, nil
	}
	switch job.JobType {
	case storage.JobTypeReview, storage.JobTypeRange, storage.JobTypeDirty, storage.JobTypeSynthesis:
	default:
		return nil, nil
	}
	hooks, err := kataPublicationHooks(cfg, job, checkout)
	if err != nil {
		return nil, err
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
		candidate := kataSelection{seed: storage.KataPublicationSeed{Endpoint: endpoint, Project: strings.TrimSpace(hook.Project), Policy: hook.Findings}, options: kata.PublicationOptions{Labels: append([]string{kata.RoborevLabel}, hook.Labels...), Priority: hook.Priority}}
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
		if selected == nil {
			selected = &candidate
			continue
		}
		if selected.seed.Policy != candidate.seed.Policy || !slices.Equal(selected.options.Labels, candidate.options.Labels) || !sameKataPriority(selected.options.Priority, candidate.options.Priority) {
			return nil, errors.New("conflicting Kata publication hooks")
		}
		if selected.seed == candidate.seed {
			continue
		}
		if selected.seed.AliasKind == "" && candidate.seed.AliasKind == "" {
			return nil, errors.New("conflicting Kata publication hooks")
		}
		uid := selected.resolvedUID
		if uid == "" {
			uid, err = resolveKataSelection(ctx, cfg.Integrations.Kata, selected.seed)
			if err != nil {
				return nil, fmt.Errorf("resolve Kata publication hook: %w", err)
			}
		}
		otherUID, err := resolveKataSelection(ctx, cfg.Integrations.Kata, candidate.seed)
		if err != nil {
			return nil, fmt.Errorf("resolve Kata publication hook: %w", err)
		}
		if uid != otherUID {
			return nil, errors.New("conflicting Kata publication hooks")
		}
		// Keep the committed alias authoritative even when the explicit hook
		// was encountered first. The hint is not used as a destination.
		if selected.seed.AliasKind == "" && candidate.seed.AliasKind != "" {
			selected = &candidate
		}
		selected.resolvedUID = uid
	}
	return selected, nil
}

func resolveKataSelection(ctx context.Context, cfg config.KataIntegrationConfig, seed storage.KataPublicationSeed) (string, error) {
	client, name, err := kataSelectionClient(cfg, seed)
	if err != nil {
		return "", err
	}
	defer client.CloseIdleConnections()
	project, err := client.ResolvePublicationProject(ctx, name)
	if err != nil {
		return "", err
	}
	return project.UID, nil
}

func kataSelectionClient(cfg config.KataIntegrationConfig, seed storage.KataPublicationSeed) (*kata.APIClient, string, error) {
	opts := []kata.APIOption{kata.WithoutWorkspace()}
	name := seed.Project
	if seed.AliasKind != "" {
		opts = []kata.APIOption{kata.WithWorkspaceAlias(kata.ProjectAlias{Kind: seed.AliasKind, Identity: seed.AliasIdentity})}
		name = ""
	}
	client, err := kata.NewAPIClient("", cfg, opts...)
	return client, name, err
}

// kataPublicationHooks applies the same supplied-review suppression to reads
// and writes. Reading chooses a project without conferring authorization.
func kataPublicationHooks(cfg *config.Config, job *storage.ReviewJob, checkout string) ([]config.HookConfig, error) {
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
	return hooks, nil
}

func kataInspectionProject(cfg *config.Config, job *storage.ReviewJob, checkout string) (string, error) {
	hooks, err := kataPublicationHooks(cfg, job, checkout)
	if err != nil {
		return "", err
	}
	var selected string
	for _, hook := range hooks {
		if hook.Type != "kata" || (hook.Findings != "comments" && hook.Findings != "summary") || !matchEvent(hook.Event, "review.completed") || !matchBranch(hook.Branches, job.HookBranch()) {
			continue
		}
		name := strings.TrimSpace(hook.Project)
		if name == "" {
			continue
		}
		if selected != "" && selected != name {
			return "", errors.New("ambiguous Kata publication projects")
		}
		selected = name
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
