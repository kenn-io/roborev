package daemon

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/review/autotype"
	"go.kenn.io/roborev/internal/storage"
)

// AutoDesignMetrics carries per-outcome counters for the auto-design router.
// Counters use atomics so producers (enqueue handler, CI poller, worker)
// don't contend on a mutex. Process-global because every producer needs to
// reach it without plumbing a reference through every code path.
type AutoDesignMetrics struct {
	triggeredHeuristic  atomic.Int64
	skippedHeuristic    atomic.Int64
	triggeredClassifier atomic.Int64
	skippedClassifier   atomic.Int64
	classifierFailed    atomic.Int64
}

// autoDesignMetrics is the process-global instance.
var autoDesignMetrics = &AutoDesignMetrics{}

const (
	autoDesignHookSource           = "post_commit"
	localAutoDesignReviewReasoning = "thorough"
)

func (s *Server) autoDesignConfig() *config.Config {
	if s != nil && s.configWatcher != nil {
		if cfg := s.configWatcher.Config(); cfg != nil {
			return cfg
		}
	}
	cfg, _ := config.LoadGlobal()
	return cfg
}

// RecordHeuristic bumps either TriggeredHeuristic or SkippedHeuristic.
func (m *AutoDesignMetrics) RecordHeuristic(run bool) {
	if run {
		m.triggeredHeuristic.Add(1)
		return
	}
	m.skippedHeuristic.Add(1)
}

// RecordClassifier bumps a classifier counter. failed=true wins over run.
func (m *AutoDesignMetrics) RecordClassifier(run, failed bool) {
	if failed {
		m.classifierFailed.Add(1)
		return
	}
	if run {
		m.triggeredClassifier.Add(1)
		return
	}
	m.skippedClassifier.Add(1)
}

// Snapshot returns a copy of the current counter values.
func (m *AutoDesignMetrics) Snapshot() storage.AutoDesignStatus {
	return storage.AutoDesignStatus{
		TriggeredHeuristic:  m.triggeredHeuristic.Load(),
		SkippedHeuristic:    m.skippedHeuristic.Load(),
		TriggeredClassifier: m.triggeredClassifier.Load(),
		SkippedClassifier:   m.skippedClassifier.Load(),
		ClassifierFailed:    m.classifierFailed.Load(),
	}
}

// AutoDesignMetricsSnapshot returns the package-global metrics snapshot.
func AutoDesignMetricsSnapshot() storage.AutoDesignStatus {
	return autoDesignMetrics.Snapshot()
}

// autoDesignStatusForResponse returns a populated AutoDesignStatus when
// auto-design is effectively enabled (globally, or by at least one
// registered repo). Returns nil otherwise so the JSON omits the field.
func (s *Server) autoDesignStatusForResponse() *storage.AutoDesignStatus {
	cfg := s.autoDesignConfig()
	enabled := cfg != nil && (cfg.AutoDesignReview.Enabled || cfg.AutoDesignReview.HookEnabled)

	if !enabled {
		repos, err := s.db.ListRepos()
		if err == nil {
			for _, r := range repos {
				repoCfg, _ := config.LoadRepoConfig(r.RootPath)
				if config.AutoDesignEnabledFromConfig(repoCfg, cfg) ||
					config.AutoDesignHookEnabledFromConfig(repoCfg, cfg) {
					enabled = true
					break
				}
			}
		}
	}

	if !enabled {
		return nil
	}
	snap := autoDesignMetrics.Snapshot()
	snap.Enabled = true
	return &snap
}

// ResetAutoDesignMetricsForTest zeroes the global counters. Tests only.
func ResetAutoDesignMetricsForTest() {
	autoDesignMetrics.triggeredHeuristic.Store(0)
	autoDesignMetrics.skippedHeuristic.Store(0)
	autoDesignMetrics.triggeredClassifier.Store(0)
	autoDesignMetrics.skippedClassifier.Store(0)
	autoDesignMetrics.classifierFailed.Store(0)
}

// maybeDispatchAutoDesign decides whether to enqueue a follow-up design
// review (or a classify job) for the just-enqueued primary review job.
// Opportunistic: errors are logged but never bubble up to the caller's HTTP
// response.
func (s *Server) maybeDispatchAutoDesign(ctx context.Context, parent *storage.ReviewJob) error {
	cfg := s.autoDesignConfig()
	if !autoDesignEnabledForSource(parent.RepoPath, parent.Source, cfg) {
		return nil
	}

	if parent.CommitID != nil {
		c, err := s.db.GetCommitByID(*parent.CommitID)
		if err == nil && c != nil {
			if has, err := s.db.HasAutoDesignSlotForCommit(parent.RepoID, c.SHA); err == nil && has {
				return nil
			}
		}
	}

	h := config.ResolveAutoDesignHeuristics(parent.RepoPath, cfg)
	if err := h.Validate(); err != nil {
		log.Printf("auto-design: invalid heuristics config for %s, skipping dispatch: %v", parent.RepoPath, err)
		return nil
	}
	hh := autotype.Heuristics{
		MinDiffLines:           h.MinDiffLines,
		LargeDiffLines:         h.LargeDiffLines,
		LargeFileCount:         h.LargeFileCount,
		TriggerPaths:           h.TriggerPaths,
		SkipPaths:              h.SkipPaths,
		TriggerMessagePatterns: h.TriggerMessagePatterns,
		SkipMessagePatterns:    h.SkipMessagePatterns,
	}

	// Resolve heuristic inputs. Per spec policy, git-lookup failures
	// on a real-commit review must degrade to classifier-ambiguous,
	// NOT to a heuristic skip decision that an empty diff would
	// naturally produce (MinDiffLines would treat 0 lines as
	// "trivial"). When DiffContent is already provided inline (dirty
	// review) or JobType isn't a single-commit review, skip git
	// entirely — the caller carries what we need and git-based
	// file/diff lookup doesn't apply.
	diff := derefString(parent.DiffContent)
	var files []string
	if diff == "" && parent.JobType == storage.JobTypeReview && parent.GitRef != "" && parent.GitRef != "dirty" {
		inputsIncomplete := false
		var err error
		diff, err = git.GetDiff(parent.RepoPath, parent.GitRef)
		if err != nil {
			log.Printf("auto-design: git.GetDiff(%s) failed, deferring to classifier: %v", parent.GitRef, err)
			inputsIncomplete = true
			diff = ""
		}
		if !inputsIncomplete {
			files, err = git.GetFilesChanged(parent.RepoPath, parent.GitRef)
			if err != nil {
				log.Printf("auto-design: git.GetFilesChanged(%s) failed, deferring to classifier: %v", parent.GitRef, err)
				inputsIncomplete = true
				files = nil
			}
		}
		if inputsIncomplete {
			return s.enqueueClassifyJob(parent)
		}
	}

	in := autotype.Input{
		RepoPath:     parent.RepoPath,
		GitRef:       parent.GitRef,
		Diff:         diff,
		Message:      classifierCommitMessage(parent.RepoPath, parent.GitRef, parent.CommitSubject),
		ChangedFiles: files,
	}

	d, err := autotype.Classify(ctx, in, hh, autotype.ErrOnClassifier{})
	switch {
	case err == nil && d.Method == autotype.MethodHeuristic:
		autoDesignMetrics.RecordHeuristic(d.Run)
		if d.Run {
			return s.enqueueDesignFollowUp(parent)
		}
		return s.insertSkippedDesign(parent, d.Reason)
	case errors.Is(err, autotype.ErrNeedsClassifier):
		return s.enqueueClassifyJob(parent)
	default:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Printf("auto-design: context ended during heuristics (%v); skipping opportunistic work", err)
			return nil
		}
		log.Printf("auto-design: Classify error, skipping: %v", err)
		return s.insertSkippedDesign(parent, "auto-design: heuristic error")
	}
}

func autoDesignEnabledForSource(repoPath, source string, cfg *config.Config) bool {
	if config.ResolveAutoDesignEnabled(repoPath, cfg) {
		return true
	}
	return source == autoDesignHookSource && config.ResolveAutoDesignHookEnabled(repoPath, cfg)
}

func (s *Server) enqueueClassifyJob(parent *storage.ReviewJob) error {
	_, err := s.db.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     parent.RepoID,
		CommitID:   parent.CommitIDValue(),
		GitRef:     parent.GitRef,
		Branch:     parent.Branch,
		JobType:    storage.JobTypeClassify,
		ReviewType: "design",
	})
	return err
}

func (s *Server) enqueueDesignFollowUp(parent *storage.ReviewJob) error {
	cfg := s.autoDesignConfig()
	designAgent, designModel := resolveDesignAgent(parent.RepoPath, cfg)
	_, err := s.db.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:     parent.RepoID,
		CommitID:   parent.CommitIDValue(),
		GitRef:     parent.GitRef,
		Branch:     parent.Branch,
		Agent:      designAgent,
		Model:      designModel,
		JobType:    storage.JobTypeReview,
		ReviewType: "design",
	})
	return err
}

// resolveDesignAgent returns the (agent, model) pair the auto-design follow-up
// should persist for execution. Workflow-specific design agents and backups
// stay strict, while generic defaults can auto-detect an installed agent like
// the normal blank-agent path.
func resolveDesignAgent(repoPath string, cfg *config.Config) (string, string) {
	repoCfg, _ := config.LoadRepoConfig(repoPath)
	return resolveDesignFollowUpAgentFromConfig(
		repoCfg, cfg, localAutoDesignReviewReasoning, "",
	)
}

func resolveDesignFollowUpAgentFromConfig(
	repoCfg *config.RepoConfig,
	cfg *config.Config,
	reasoning string,
	modelOverride string,
) (string, string) {
	resolution, err := agent.ResolveWorkflowConfigFromConfig(
		"", repoCfg, cfg, "design", reasoning)
	if err != nil || resolution.PreferredAgent == "" {
		designAgent, designModel := config.DesignAgentFromConfig(repoCfg, cfg)
		if modelOverride != "" {
			designModel = modelOverride
		}
		return designAgent, designModel
	}
	primary := resolution.PreferredAgent
	strictDesignAgent := config.HasWorkflowAgentOverrideFromConfig(
		repoCfg, cfg, "design", reasoning,
	) ||
		strings.TrimSpace(resolution.BackupAgent) != ""
	var chosen agent.Agent
	if strictDesignAgent {
		chosen, err = agent.GetPreferredOrBackupWithConfigFromConfig(
			repoCfg, primary, cfg, resolution.BackupAgent,
		)
	} else {
		chosen, err = agent.GetAvailableWithConfigFromConfig(repoCfg, primary, cfg)
	}
	if err != nil {
		// Nothing installed — fall back to the primary name anyway so
		// the row has a readable agent value, even if the worker will
		// error. Better than persisting the sentinel.
		return primary, resolution.ModelForSelectedAgent(primary, modelOverride)
	}
	return chosen.Name(), resolution.ModelForSelectedAgent(chosen.Name(), modelOverride)
}

func (s *Server) insertSkippedDesign(parent *storage.ReviewJob, reason string) error {
	return s.db.InsertSkippedDesignJob(storage.InsertSkippedDesignJobParams{
		RepoID:     parent.RepoID,
		CommitID:   parent.CommitIDValue(),
		GitRef:     parent.GitRef,
		Branch:     parent.Branch,
		SkipReason: reason,
	})
}
