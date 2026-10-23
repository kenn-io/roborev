package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/storage"
)

func kataEligibleJob(job *storage.ReviewJob) bool {
	return job != nil && !job.IsCIReview() && job.PanelRole != storage.PanelRoleMember && job.Source != storage.JobSourceRebuttal && (job.IsReviewJob() || job.IsSynthesisJob())
}

type kataDestinationResolver func(context.Context, string, string) (kata.Binding, error)

// buildKataPublications runs before the SQLite transition: resolve a destination
// here, then persist these exact payloads together with the immutable findings.
func buildKataPublications(ctx context.Context, cfg *config.Config, c storage.KataCompletion, resolve kataDestinationResolver) ([]katapublish.Request, error) {
	builder, err := prepareKataPublications(ctx, cfg, c.Job, resolve)
	if err != nil {
		return nil, err
	}
	return builder(ctx, c)
}

// prepareKataPublications freezes configuration and resolves destinations before
// a worker acquires either attempt-transition lock. The returned builder only
// assembles local data, including when a completion must instead fail.
func prepareKataPublications(ctx context.Context, cfg *config.Config, job *storage.ReviewJob, resolve kataDestinationResolver) (storage.KataHandoffBuilder, error) {
	noop := func(context.Context, storage.KataCompletion) ([]katapublish.Request, error) { return nil, nil }
	if cfg == nil || !cfg.Integrations.Kata.Enabled || !kataEligibleJob(job) {
		return noop, nil
	}
	connection := cfg.Integrations.Kata
	effectiveRepo := resolveEffectiveRepoPath("kata-publication", job)
	hooks := append([]config.HookConfig{}, cfg.Hooks...)
	if repo, err := config.LoadRepoConfig(effectiveRepo); err != nil {
		return nil, err
	} else if repo != nil {
		hooks = append(hooks, repo.Hooks...)
	}
	if err := config.ValidateKataHooks(hooks, true); err != nil {
		return nil, err
	}
	type destinationHook struct {
		hook                                                config.HookConfig
		endpoint, project, projectUID, credential, teammate string
	}
	var captured []destinationHook
	bindings := map[string]kata.Binding{}
	var credentialFingerprint string
	for _, hook := range hooks {
		if hook.Type != "kata" || !matchBranch(hook.Branches, job.HookBranch()) ||
			(!matchEvent(hook.Event, jobEventType(job, "completed")) && !matchEvent(hook.Event, jobEventType(job, "failed"))) {
			continue
		}
		project := hook.Project
		if project == "" {
			b, err := kata.NewCLIClient(effectiveRepo).Binding(ctx)
			if err == nil {
				project = b.Project
			} else if !errors.Is(err, kata.ErrNoBinding) {
				return nil, err
			} else {
				project = connection.DefaultProject
			}
		}
		if project == "" {
			continue
		}
		endpoint, err := kata.NormalizeEndpoint(connection.Endpoint)
		if err != nil {
			return nil, err
		}
		if credentialFingerprint == "" {
			credential, err := connection.ResolveAPIKey()
			if err != nil {
				return nil, fmt.Errorf("kata credential unavailable")
			}
			credentialDigest := sha256.Sum256([]byte(credential))
			credentialFingerprint = hex.EncodeToString(credentialDigest[:])
		}
		binding, found := bindings[project]
		if !found {
			binding, err = resolve(ctx, endpoint, project)
			if err != nil {
				binding = kata.Binding{Project: project}
			}
			bindings[project] = binding
		}
		// Copy slices and pointers supplied by configuration so a reload cannot
		// alter the policy after destination preparation.
		hook.Labels = append([]string(nil), hook.Labels...)
		if hook.Priority != nil {
			priority := *hook.Priority
			hook.Priority = &priority
		}
		captured = append(captured, destinationHook{hook, endpoint, project, binding.ProjectUID, credentialFingerprint, connection.ResolvedTeammate()})
	}
	return func(_ context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
		eventType := jobEventType(c.Job, "completed")
		if c.Error != "" {
			eventType = jobEventType(c.Job, "failed")
		}
		event := eventForJob(eventType, c.Job, c.Job.ID)
		event.TS, event.Findings, event.Error, event.Verdict = c.At, c.Output, c.Error, string(c.Verdict)
		var requests []katapublish.Request
		seen := map[string]struct{}{}
		for _, destination := range captured {
			hook := destination.hook
			if !matchEvent(hook.Event, eventType) {
				continue
			}
			filtered := event
			if hook.Findings == "summary" {
				filtered.Findings = "Review findings are available locally."
				filtered.Error = ""
			}
			create, ok := kataCreateRequest(hook, filtered)
			if !ok {
				continue
			}
			if hook.Findings == "comments" && len(c.Snapshots) > 0 {
				filtered.Findings = "Findings are recorded as comments below."
				create.Body = buildKataHookBody(filtered)
			}
			policy := hook.Findings
			if policy == "" {
				policy = "legacy"
			}
			identity := c.ReviewUUID
			if identity == "" {
				identity = storage.KataFailureRun(c.Job)
			}
			key, err := json.Marshal(struct {
				Identity, Event, Endpoint, Project, Policy string
				Labels                                     []string
				Priority                                   *int
			}{identity, eventType, destination.endpoint, destination.project, policy, create.Labels, create.Priority}, json.Deterministic(true))
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(key)
			operationID := "publish:" + hex.EncodeToString(digest[:])
			if _, exists := seen[operationID]; exists {
				continue
			}
			seen[operationID] = struct{}{}
			create.Project, create.ForceNew = destination.project, true
			req := katapublish.Request{OperationID: operationID, Endpoint: destination.endpoint, Project: destination.project, ProjectUID: destination.projectUID, Policy: policy, ReviewUUID: c.ReviewUUID, Teammate: destination.teammate, CredentialFingerprint: destination.credential, Create: create}
			if policy != "legacy" {
				for _, snap := range c.Snapshots {
					body := fmt.Sprintf("Finding %s. Inspect with roborev show %d.", snap.Selector, c.Job.ID)
					if policy == "comments" {
						body = reviewFindingFromSnapshot(snap).Excerpt()
					}
					req.Entries = append(req.Entries, katapublish.Entry{Selector: snap.Selector, ReviewUUID: snap.ReviewUUID.String(), DocumentHash: snap.DocumentHash, Index: snap.Index, Body: body})
				}
			}
			requests = append(requests, req)
		}
		return requests, nil
	}, nil
}

// kataPublisher drains only saved operations. No event or CLI wake-up is needed
// for retries, and current hooks never authorize old reviews at daemon startup.
type kataPublisher struct {
	db     *storage.DB
	config ConfigGetter
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	newAPI func(config.KataIntegrationConfig) (katapublish.API, error)
}

func (p *kataPublisher) start(parent context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			for ctx.Err() == nil {
				h, err := p.db.ClaimKataHandoff(ctx, time.Now())
				if err != nil {
					log.Printf("Kata delivery: claim failed")
					break
				}
				if h == nil {
					break
				}
				p.deliver(ctx, *h)
			}
			// Polling is local queue latency, not a transport timeout or a data bound.
			timer.Reset(time.Second)
		}
	}()
}

func (p *kataPublisher) stop() {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (p *kataPublisher) deliver(ctx context.Context, h storage.KataHandoff) {
	cfg, deliveryErr := kataPublicationConnection(p.config.Config().Integrations.Kata, h.Request)
	if deliveryErr == nil {
		var client katapublish.API
		var err error
		if p.newAPI != nil {
			client, err = p.newAPI(cfg)
		} else {
			client, err = kata.NewAPIClient("", cfg, kata.WithoutWorkspace())
		}
		if err != nil {
			deliveryErr = fmt.Errorf("kata connection unavailable")
		} else {
			h.State, deliveryErr = katapublish.Deliver(ctx, client, h.Request, h.State, func(state katapublish.State) error {
				return p.db.CheckpointKataHandoff(ctx, h.OperationID, h.Claim, state)
			})
		}
	}
	// Never retain raw provider text: error responses can echo review payloads.
	// katapublish returns controlled diagnostics and strips raw provider messages.
	if err := p.db.FinishKataHandoff(context.WithoutCancel(ctx), h, deliveryErr, time.Now()); err != nil {
		log.Printf("Kata delivery: checkpoint failed")
	}
}

type kataTransition struct {
	builder storage.KataHandoffBuilder
	mode    string
}

func (s *Server) prepareKataTransition(ctx context.Context, job *storage.ReviewJob) kataTransition {
	cfg := s.configWatcher.Config()
	mode := "cli"
	if cfg != nil && cfg.Integrations.Kata.Enabled {
		mode = "api"
	}
	builder, err := prepareKataPublications(ctx, cfg, job, func(ctx context.Context, endpoint, project string) (kata.Binding, error) {
		client, err := kata.NewAPIClient("", cfg.Integrations.Kata, kata.WithoutWorkspace())
		if err == nil {
			var binding kata.Binding
			binding, err = client.ResolveProject(ctx, project)
			if err == nil {
				if err = s.db.RememberKataDestination(ctx, endpoint, project, binding.ProjectUID); err != nil {
					return kata.Binding{}, err
				}
				return binding, nil
			}
		}
		uid, cacheErr := s.db.CachedKataDestination(ctx, endpoint, project)
		if cacheErr == nil && uid != "" {
			return kata.Binding{Project: project, ProjectUID: uid}, nil
		}
		return kata.Binding{}, err
	})
	if err != nil {
		builder = func(context.Context, storage.KataCompletion) ([]katapublish.Request, error) { return nil, err }
	}
	return kataTransition{builder: builder, mode: mode}
}

func (s *Server) prepareKataPublication(ctx context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
	return s.prepareKataTransition(ctx, c.Job).builder(ctx, c)
}

func kataCredentialFingerprint(cfg config.KataIntegrationConfig) string {
	key, err := cfg.ResolveAPIKey()
	if err != nil {
		return "unavailable"
	}
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// Resolve a key file once. The same checked bytes construct the transport, so
// an atomic rotation cannot substitute a different credential after checking.
func kataPublicationConnection(cfg config.KataIntegrationConfig, req katapublish.Request) (config.KataIntegrationConfig, error) {
	if !cfg.Enabled {
		return cfg, fmt.Errorf("kata connection disabled")
	}
	endpoint, err := kata.NormalizeEndpoint(cfg.Endpoint)
	if err != nil || endpoint != req.Endpoint {
		return cfg, fmt.Errorf("kata destination endpoint changed")
	}
	key, err := cfg.ResolveAPIKey()
	if err != nil {
		return cfg, fmt.Errorf("kata credential unavailable")
	}
	digest := sha256.Sum256([]byte(key))
	if hex.EncodeToString(digest[:]) != req.CredentialFingerprint {
		return cfg, fmt.Errorf("kata credential changed")
	}
	cfg.APIKey = key
	cfg.APIKeyFile = ""
	return cfg, nil
}
