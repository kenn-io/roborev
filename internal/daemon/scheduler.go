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
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/storage"
)

const schedulerControlInterval = time.Second

type SchedulerService struct {
	db         *storage.DB
	cfg        *ConfigWatcher
	now        func() time.Time
	stop       context.CancelFunc
	stopped    chan struct{}
	mu         sync.Mutex
	running    bool
	started    bool
	due        map[int64]time.Time
	lastRun    map[int64]time.Time
	lastReload uint64
	validRepo  map[int64]repoScheduleState
	stopping   bool
}

type repoScheduleState struct {
	cfg    *config.RepoConfig
	raw    map[string]any
	policy config.EffectiveSchedule
}

func NewSchedulerService(db *storage.DB, cfg *ConfigWatcher) *SchedulerService {
	return &SchedulerService{db: db, cfg: cfg, now: time.Now, stopped: make(chan struct{}), due: make(map[int64]time.Time), lastRun: make(map[int64]time.Time), validRepo: make(map[int64]repoScheduleState)}
}

func (s *SchedulerService) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	ctx, s.stop = context.WithCancel(ctx)
	go s.loop(ctx)
}

func (s *SchedulerService) Stop() {
	s.mu.Lock()
	started := s.started
	stop := s.stop
	s.stopping = true
	s.mu.Unlock()
	if !started {
		return
	}
	if stop != nil {
		stop()
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
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	repos, err := s.db.ListRepos()
	if err != nil {
		log.Printf("scheduled analysis: list repositories: %v", err)
		return
	}
	global := s.cfg.Config()
	reloaded := s.cfg.ReloadCounter() != s.lastReload
	s.lastReload = s.cfg.ReloadCounter()
	seen := make(map[int64]bool)
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		repoCfg, raw, err := config.LoadRepoConfigWithRaw(repo.RootPath)
		if err != nil {
			if cached, ok := s.validRepo[repo.ID]; ok {
				repoCfg, raw = cached.cfg, cached.raw
			} else {
				log.Printf("scheduled analysis: load %s: %v", repo.RootPath, err)
				continue
			}
		} else if repoCfg == nil {
			delete(s.validRepo, repo.ID)
		} else {
			previous := s.validRepo[repo.ID]
			policy := config.MergeSchedule(global.Schedule, scheduleOrEmpty(repoCfg), raw)
			s.validRepo[repo.ID] = repoScheduleState{cfg: repoCfg, raw: raw, policy: policy}
			if previous.cfg == nil || !reflect.DeepEqual(previous.policy, policy) || reloaded {
				s.due[repo.ID] = s.now()
			}
		}
		policy := config.MergeSchedule(global.Schedule, scheduleOrEmpty(repoCfg), raw)
		if !policy.Enabled || repoCfg == nil || repoCfg.Schedule.Enabled == nil || !*repoCfg.Schedule.Enabled {
			delete(s.due, repo.ID)
			continue
		}
		if policy.MaxFiles <= 0 {
			log.Printf("scheduled analysis: %s: schedule.max_files must be positive", repo.RootPath)
			continue
		}
		seen[repo.ID] = true
		if policy.Interval <= 0 {
			policy.Interval = time.Hour
		}
		if _, ok := s.due[repo.ID]; !ok {
			s.due[repo.ID] = s.now()
		}
		if s.now().Before(s.due[repo.ID]) {
			continue
		}
		if err := s.runRepo(ctx, repo, repoCfg, raw, global, policy); err != nil {
			log.Printf("scheduled analysis: %s: %v", repo.RootPath, err)
		}
		s.due[repo.ID] = s.now().Add(policy.Interval)
		s.lastRun[repo.ID] = s.now()
	}
	for id := range s.due {
		if !seen[id] {
			delete(s.due, id)
		}
	}
}

func scheduleOrEmpty(c *config.RepoConfig) config.ScheduleConfig {
	if c == nil {
		return config.ScheduleConfig{}
	}
	return c.Schedule
}

type scheduledCandidate struct {
	path, typ string
	when      time.Time
	id        int64
}

func (s *SchedulerService) runRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, raw map[string]any, global *config.Config, policy config.EffectiveSchedule) error {
	sha, err := git.CurrentHeadSHA(ctx, repo.RootPath)
	if err != nil {
		return err
	}
	files, err := git.TrackedFilesAt(ctx, repo.RootPath, sha)
	if err != nil {
		return err
	}
	types := append([]string(nil), policy.Types...)
	if len(types) == 0 {
		for _, typ := range analyze.AllTypes {
			types = append(types, typ.Name)
		}
	}
	var candidates []scheduledCandidate
	for _, path := range files {
		if !git.IsSourceFile(path) || !scheduledPathMatch(path, policy.Paths) {
			continue
		}
		for _, typ := range types {
			history, err := s.db.ScheduledAnalysisHistory(repo.ID, path, typ)
			if err != nil {
				return err
			}
			var baseline *storage.ScheduledAnalysisRecord
			pending := false
			for i := range history {
				r := history[i]
				if (r.Status == storage.JobStatusQueued || r.Status == storage.JobStatusRunning) && r.Commit == sha {
					pending = true
				}
				if r.Status == storage.JobStatusDone && r.Commit != "" {
					if baseline == nil || r.FinishedAtOrEnqueued().After(baseline.FinishedAtOrEnqueued()) {
						x := r
						baseline = &x
					}
				}
			}
			if pending || (baseline != nil && baseline.Commit == sha) {
				continue
			}
			when := time.Time{}
			if baseline != nil {
				when = baseline.FinishedAtOrEnqueued()
				if git.FilesUnchangedBetween(ctx, repo.RootPath, baseline.Commit, sha, path) {
					continue
				}
			}
			candidates = append(candidates, scheduledCandidate{path: path, typ: typ, when: when, id: recordID(baseline)})
		}
	}
	sortScheduledCandidates(candidates)
	for _, c := range selectScheduledCandidates(candidates, policy.MaxFiles) {
		if err := s.enqueue(ctx, repo, repoCfg, raw, global, policy, c, sha); err != nil {
			log.Printf("scheduled analysis: enqueue %s: %v", c.path, err)
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
	seen := map[string]bool{}
	selected := make([]scheduledCandidate, 0, maxFiles)
	count := 0
	for _, c := range candidates {
		if seen[c.path] {
			continue
		}
		if count >= maxFiles {
			break
		}
		seen[c.path] = true
		count++
		selected = append(selected, c)
	}
	return selected
}

func recordID(r *storage.ScheduledAnalysisRecord) int64 {
	if r == nil {
		return 0
	}
	return r.ID
}

func scheduledPathMatch(path string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	path = filepath.ToSlash(path)
	for _, f := range filters {
		f = strings.Trim(filepath.ToSlash(f), "/")
		if f == "" || path == f || strings.HasPrefix(path, f+"/") {
			return true
		}
	}
	return false
}

func (s *SchedulerService) enqueue(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, raw map[string]any, global *config.Config, policy config.EffectiveSchedule, c scheduledCandidate, sha string) error {
	s.mu.Lock()
	if s.stopping || ctx.Err() != nil {
		s.mu.Unlock()
		return context.Canceled
	}
	defer s.mu.Unlock()
	t := analyze.GetType(c.typ)
	if t == nil {
		return fmt.Errorf("unknown analysis type %q", c.typ)
	}
	content, err := git.ReadBlobAt(ctx, repo.RootPath, sha, c.path)
	if err != nil {
		return err
	}
	promptText, err := t.BuildPrompt(map[string]string{c.path: content})
	if err != nil {
		return err
	}
	resolved, err := config.ResolveScheduledAnalyzeConfigFromConfig(repoCfg, global.ForRepo(repo.RootPath), c.typ)
	if err != nil {
		return err
	}
	_, err = s.db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: c.typ, Agent: resolved.Agent, Model: resolved.Model, Reasoning: resolved.Reasoning, Prompt: promptText, PromptPrebuilt: true, Agentic: false, OutputPrefix: analyze.BuildOutputPrefix(c.typ, []string{c.path}), AnalysisType: c.typ, AnalysisFiles: []string{c.path}, AnalysisCommitSHA: sha, JobType: storage.JobTypeTask, Source: storage.JobSourceScheduled, Label: c.typ})
	return err
}
