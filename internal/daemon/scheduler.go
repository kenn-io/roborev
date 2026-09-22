package daemon

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/storage"
)

const schedulerControlInterval = time.Second

type ScheduledEnqueueFunc func(context.Context, storage.Repo, storage.EnqueueOpts) (*storage.ReviewJob, error)

type SchedulerService struct {
	db          *storage.DB
	cfg         *ConfigWatcher
	now         func() time.Time
	enqueueFunc ScheduledEnqueueFunc
	stop        context.CancelFunc
	stopped     chan struct{}
	mu          sync.Mutex
	admissionMu sync.Mutex
	running     bool
	started     bool
	due         map[int64]time.Time
	validRepo   map[int64]repoScheduleState
	rejected    map[int64]string
	stopping    atomic.Bool
}

type repoScheduleState struct {
	cfg    *config.RepoConfig
	raw    map[string]any
	policy config.EffectiveSchedule
}

func NewSchedulerService(db *storage.DB, cfg *ConfigWatcher, enqueue ScheduledEnqueueFunc) *SchedulerService {
	return &SchedulerService{
		db:          db,
		cfg:         cfg,
		now:         time.Now,
		enqueueFunc: enqueue,
		stopped:     make(chan struct{}),
		due:         make(map[int64]time.Time),
		validRepo:   make(map[int64]repoScheduleState),
		rejected:    make(map[int64]string),
	}
}

func (s *SchedulerService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.stopping.Load() {
		s.mu.Unlock()
		return
	}
	ctx, s.stop = context.WithCancel(ctx)
	s.started = true
	s.mu.Unlock()
	go s.loop(ctx)
}

// BeginStop closes scheduled enqueue admission and cancels the scheduler loop.
// It returns after any enqueue already admitted has completed.
func (s *SchedulerService) BeginStop() {
	s.mu.Lock()
	if !s.started {
		s.stopping.Store(true)
		s.mu.Unlock()
		return
	}
	stop := s.stop
	s.mu.Unlock()
	s.admissionMu.Lock()
	s.stopping.Store(true)
	s.admissionMu.Unlock()
	stop()
}

func (s *SchedulerService) Stop() {
	s.BeginStop()
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return
	}
	<-s.stopped
}

func (s *SchedulerService) loop(ctx context.Context) {
	defer close(s.stopped)
	t := time.NewTicker(schedulerControlInterval)
	defer t.Stop()
	s.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcile(ctx)
		}
	}
}

func (s *SchedulerService) reconcile(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	repos, err := s.db.ListRepos()
	if err != nil {
		log.Printf("scheduled analysis: list repositories: %v", err)
		return
	}
	global := s.cfg.Config()
	if global == nil {
		return
	}
	now := s.now()
	seen := make(map[int64]bool, len(repos))
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}

		repoCfg, raw, err := config.LoadRepoConfigWithRaw(repo.RootPath)
		if err != nil {
			cached, ok := s.validRepo[repo.ID]
			if !ok {
				s.reject(repo.ID, err)
				continue
			}
			repoCfg, raw = cached.cfg, cached.raw
		}
		if repoCfg == nil {
			delete(s.validRepo, repo.ID)
			delete(s.due, repo.ID)
			delete(s.rejected, repo.ID)
			continue
		}

		scopedGlobal := global.ForRepo(repo.RootPath)
		policy := config.MergeSchedule(scopedGlobal.Schedule, repoCfg.Schedule, raw)
		if scheduleOptedIn(scopedGlobal, repoCfg) {
			if err := policy.Validate(); err != nil {
				cached, ok := s.validRepo[repo.ID]
				if ok {
					fallback := config.MergeSchedule(scopedGlobal.Schedule, cached.cfg.Schedule, cached.raw)
					if fallback.Validate() == nil {
						repoCfg, raw, policy = cached.cfg, cached.raw, fallback
					} else {
						s.reject(repo.ID, err)
						continue
					}
				} else {
					s.reject(repo.ID, err)
					continue
				}
			}
		}
		delete(s.rejected, repo.ID)

		previous, hadPrevious := s.validRepo[repo.ID]
		s.validRepo[repo.ID] = repoScheduleState{cfg: repoCfg, raw: raw, policy: policy}
		if !policy.Enabled || !scheduleOptedIn(scopedGlobal, repoCfg) {
			delete(s.due, repo.ID)
			continue
		}
		seen[repo.ID] = true
		if !hadPrevious || !reflect.DeepEqual(previous.policy, policy) {
			s.due[repo.ID] = now
		}
		if due, ok := s.due[repo.ID]; !ok {
			s.due[repo.ID] = now
		} else if now.Before(due) {
			continue
		}
		if err := s.runRepo(ctx, repo, repoCfg, scopedGlobal, policy); err != nil {
			log.Printf("scheduled analysis: %s: %v", repo.RootPath, err)
		}
		s.due[repo.ID] = now.Add(policy.Interval)
	}
	for id := range s.due {
		if !seen[id] {
			delete(s.due, id)
		}
	}
}

func (s *SchedulerService) reject(repoID int64, err error) {
	message := err.Error()
	if s.rejected[repoID] == message {
		return
	}
	s.rejected[repoID] = message
	log.Printf("scheduled analysis: repository %d: %s", repoID, message)
}

func scheduleOptedIn(global *config.Config, repo *config.RepoConfig) bool {
	return global != nil && global.Schedule.Enabled != nil && *global.Schedule.Enabled &&
		repo != nil && repo.Schedule.Enabled != nil && *repo.Schedule.Enabled
}

type scheduledCandidate struct {
	path, typ string
	when      time.Time
}

func (s *SchedulerService) runRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, policy config.EffectiveSchedule) error {
	sha, err := git.CurrentHeadSHA(ctx, repo.RootPath)
	if err != nil {
		return err
	}
	files, err := git.TrackedFilesAt(ctx, repo.RootPath, sha)
	if err != nil {
		return err
	}
	history, err := s.db.ScheduledAnalysisHistoryForRepo(repo.ID)
	if err != nil {
		return err
	}
	changed := make(map[string]map[string]struct{})
	types := make([]string, 0, len(policy.Types))
	seenTypes := make(map[string]struct{}, len(policy.Types))
	for _, rawType := range policy.Types {
		typ := strings.TrimSpace(rawType)
		if _, seen := seenTypes[typ]; seen {
			continue
		}
		seenTypes[typ] = struct{}{}
		types = append(types, typ)
	}
	var candidates []scheduledCandidate
	for _, path := range files {
		if !git.IsSourceFile(path) || !scheduledPathMatch(path, policy.Paths) {
			continue
		}
		for _, typ := range types {
			typ = strings.TrimSpace(typ)
			records := history[storage.ScheduledAnalysisKey{Path: path, Type: typ}]
			var baseline *storage.ScheduledAnalysisRecord
			pending := false
			for i := range records {
				record := records[i]
				if (record.Status == storage.JobStatusQueued || record.Status == storage.JobStatusRunning) && record.Commit == sha {
					pending = true
				}
				if record.Status != storage.JobStatusDone || record.Commit == "" {
					continue
				}
				if baseline == nil || record.FinishedAtOrEnqueued().After(baseline.FinishedAtOrEnqueued()) ||
					(record.FinishedAtOrEnqueued().Equal(baseline.FinishedAtOrEnqueued()) && record.ID > baseline.ID) {
					copy := record
					baseline = &copy
				}
			}
			if pending || (baseline != nil && baseline.Commit == sha) {
				continue
			}
			when := time.Time{}
			if baseline != nil {
				when = baseline.FinishedAtOrEnqueued()
				commitExists, existsErr := git.CommitExists(ctx, repo.RootPath, baseline.Commit)
				if existsErr != nil {
					return existsErr
				}
				if !commitExists {
					candidates = append(candidates, scheduledCandidate{path: path, typ: typ})
					continue
				}
				filesForBaseline, ok := changed[baseline.Commit]
				if !ok {
					filesForBaseline, err = git.ChangedFilesBetween(ctx, repo.RootPath, baseline.Commit, sha)
					if err != nil {
						return err
					}
					changed[baseline.Commit] = filesForBaseline
				}
				if _, ok := filesForBaseline[path]; !ok {
					continue
				}
			}
			candidates = append(candidates, scheduledCandidate{path: path, typ: typ, when: when})
		}
	}
	sortScheduledCandidates(candidates)
	for _, candidate := range selectScheduledCandidates(candidates, policy.MaxFiles) {
		if err := s.enqueue(ctx, repo, repoCfg, global, candidate, sha); err != nil {
			log.Printf("scheduled analysis: enqueue %s: %v", candidate.path, err)
		}
	}
	return nil
}

func sortScheduledCandidates(candidates []scheduledCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].when.Equal(candidates[j].when) {
			if candidates[i].path == candidates[j].path {
				return candidates[i].typ < candidates[j].typ
			}
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].when.Before(candidates[j].when)
	})
}

func selectScheduledCandidates(candidates []scheduledCandidate, maxFiles int) []scheduledCandidate {
	if maxFiles <= 0 {
		return nil
	}
	paths := make(map[string]struct{}, maxFiles)
	for _, candidate := range candidates {
		if _, ok := paths[candidate.path]; ok || len(paths) >= maxFiles {
			continue
		}
		paths[candidate.path] = struct{}{}
	}
	selected := make([]scheduledCandidate, 0, len(paths))
	for _, candidate := range candidates {
		if _, ok := paths[candidate.path]; ok {
			selected = append(selected, candidate)
		}
	}
	return selected
}

func scheduledPathMatch(path string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	path = filepath.ToSlash(path)
	for _, filter := range filters {
		filter = strings.Trim(filepath.ToSlash(filter), "/")
		if filter == "" || path == filter || strings.HasPrefix(path, filter+"/") {
			return true
		}
	}
	return false
}

func (s *SchedulerService) enqueue(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, candidate scheduledCandidate, sha string) error {
	typ := analyze.GetType(candidate.typ)
	if typ == nil {
		return fmt.Errorf("unknown analysis type %q", candidate.typ)
	}
	content, err := git.ReadBlobAt(ctx, repo.RootPath, sha, candidate.path)
	if err != nil {
		return err
	}
	promptText, err := typ.BuildPrompt(map[string]string{candidate.path: content})
	if err != nil {
		return err
	}
	resolved, err := config.ResolveScheduledAnalyzeConfigFromConfig(repoCfg, global, candidate.typ)
	if err != nil {
		return err
	}
	reviewType := ""
	if candidate.typ == config.ReviewTypeSecurity {
		reviewType = config.ReviewTypeSecurity
	}
	opts := storage.EnqueueOpts{
		RepoID:            repo.ID,
		GitRef:            candidate.typ,
		Agent:             resolved.Agent,
		Model:             resolved.Model,
		Reasoning:         resolved.Reasoning,
		Prompt:            promptText,
		Agentic:           false,
		ReviewType:        reviewType,
		OutputPrefix:      analyze.BuildOutputPrefix(candidate.typ, []string{candidate.path}),
		AnalysisType:      candidate.typ,
		AnalysisFiles:     []string{candidate.path},
		AnalysisCommitSHA: sha,
		RequestedModel:    resolved.Model,
		JobType:           storage.JobTypeTask,
		Source:            storage.JobSourceScheduled,
		Label:             candidate.typ,
	}

	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.stopping.Load() || ctx.Err() != nil {
		return context.Canceled
	}
	_, err = s.enqueueFunc(ctx, repo, opts)
	return err
}
