package daemon

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/review/autotype"
	"go.kenn.io/roborev/internal/storage"
)

type analysisScheduler struct {
	server  *Server
	lastRun map[int64]time.Time
}

func (s *Server) startAnalysisScheduler(ctx context.Context) {
	s.startScheduler(ctx, &analysisScheduler{server: s, lastRun: map[int64]time.Time{}})
}

func (s *Server) startScheduler(ctx context.Context, scheduler *analysisScheduler) {
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	if s.scheduleCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.scheduleCancel, s.scheduleDone = cancel, done
	go func() { defer close(done); scheduler.run(ctx) }()
}

func (s *Server) stopAnalysisScheduler() {
	s.scheduleMu.Lock()
	cancel, done := s.scheduleCancel, s.scheduleDone
	s.scheduleCancel, s.scheduleDone = nil, nil
	s.scheduleMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (w *analysisScheduler) run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		w.poll(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *analysisScheduler) poll(ctx context.Context, now time.Time) {
	repos, err := w.server.db.ListRepos()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("analysis scheduler: %v", err)
		}
		return
	}
	global := w.server.configWatcher.Config()
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		repoCfg, err := config.LoadRepoConfig(repo.RootPath)
		if err == nil && repoCfg == nil {
			repoCfg = &config.RepoConfig{}
		}
		if err == nil {
			var schedule config.Schedule
			schedule, err = config.ResolveSchedule(global.Schedule, repoCfg.Schedule)
			if err == nil && schedule.Enabled {
				err = w.runDue(ctx, repo, repoCfg, global.ForRepo(repo.RootPath), schedule, now)
			} else if err == nil {
				err = w.server.db.ClearAnalysisScheduleLastRun(ctx, repo.ID)
				if err == nil {
					delete(w.lastRun, repo.ID)
				}
			}
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("analysis scheduler %s: %v", repo.RootPath, err)
		}
	}
}

func (w *analysisScheduler) runDue(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule, now time.Time) error {
	last, loaded := w.lastRun[repo.ID]
	if !loaded {
		var err error
		last, err = w.server.db.AnalysisScheduleLastRun(ctx, repo.ID)
		if err != nil {
			return err
		}
		w.lastRun[repo.ID] = last
	}
	if !last.IsZero() && now.Sub(last) < schedule.Interval {
		return nil
	}
	if err := w.scheduleRepo(ctx, repo, repoCfg, global, schedule); err != nil {
		return err
	}
	if err := w.server.db.SetAnalysisScheduleLastRun(ctx, repo.ID, now); err != nil {
		return err
	}
	w.lastRun[repo.ID] = now
	return nil
}

type changeSet struct {
	all   bool
	paths map[string]struct{}
}

type analysisCandidate struct {
	path   string
	due    map[string]bool
	oldest time.Time
}

func (w *analysisScheduler) scheduleRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule) error {
	head, err := git.ResolveCommitCtx(ctx, repo.RootPath, "HEAD")
	if err != nil {
		return err
	}
	paths, err := git.ListRefFiles(ctx, repo.RootPath, head)
	if err != nil {
		return err
	}
	candidates := map[string]*analysisCandidate{}
	for _, path := range paths {
		if len(schedule.Paths) > 0 {
			matched := false
			for _, pattern := range schedule.Paths {
				if strings.HasSuffix(pattern, "/") {
					pattern += "**"
				}
				ok, err := autotype.AnyMatch([]string{pattern}, path)
				if err != nil {
					return err
				}
				if ok {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		candidates[path] = &analysisCandidate{path: path, due: map[string]bool{}}
	}
	for _, typ := range schedule.Types {
		jobs, err := w.server.db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{repo.ID}), storage.WithAnalysisType(typ), storage.WithoutPrompt())
		if err != nil {
			return err
		}
		pending := map[string]bool{}
		baseline := map[string]storage.ReviewJob{}
		for _, job := range jobs {
			for _, path := range job.AnalysisFiles {
				if job.Status == storage.JobStatusQueued || job.Status == storage.JobStatusRunning {
					pending[path] = true
				}
				if job.Status == storage.JobStatusDone && job.AnalysisCommitSHA != "" {
					if _, ok := baseline[path]; !ok {
						baseline[path] = job
					}
				}
			}
		}
		changes := map[string]changeSet{}
		for path, candidate := range candidates {
			if pending[path] {
				continue
			}
			job, ok := baseline[path]
			due := !ok
			if ok {
				changed, cached := changes[job.AnalysisCommitSHA]
				if !cached {
					changed.paths = map[string]struct{}{}
					if _, err := git.ResolveCommitCtx(ctx, repo.RootPath, job.AnalysisCommitSHA); err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						changed.all = true
					} else {
						paths, err := git.DiffPathsCtx(ctx, repo.RootPath, job.AnalysisCommitSHA+".."+head, nil)
						if err != nil {
							return err
						}
						for _, p := range paths {
							changed.paths[p] = struct{}{}
						}
					}
					changes[job.AnalysisCommitSHA] = changed
				}
				_, modified := changed.paths[path]
				due = changed.all || modified
			}
			if due {
				candidate.due[typ] = true
				if !ok {
					candidate.oldest = time.Time{}
				} else if len(candidate.due) == 1 || (!candidate.oldest.IsZero() && job.EnqueuedAt.Before(candidate.oldest)) {
					candidate.oldest = job.EnqueuedAt
				}
			}
		}
	}
	var selected []*analysisCandidate
	for _, c := range candidates {
		if len(c.due) > 0 {
			selected = append(selected, c)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].oldest.Equal(selected[j].oldest) {
			return selected[i].path < selected[j].path
		}
		return selected[i].oldest.Before(selected[j].oldest)
	})
	if len(selected) > schedule.MaxFiles {
		selected = selected[:schedule.MaxFiles]
	}
	for _, typ := range schedule.Types {
		var names []string
		for _, c := range selected {
			if c.due[typ] {
				names = append(names, c.path)
			}
		}
		if len(names) == 0 {
			continue
		}
		resolved, err := config.ResolveAnalyzeConfigFromConfig("", "", "", repoCfg, global, typ, analyze.Workflow(typ), "")
		if err != nil {
			return err
		}
		prompt, err := analyze.GetType(typ).BuildPromptWithPaths(".", names)
		if err != nil {
			return err
		}
		result, err := w.server.humaEnqueue(ctx, &EnqueueInput{Body: EnqueueRequest{RepoPath: repo.RootPath, GitRef: typ, Agent: resolved.Agent, Model: resolved.Model, Reasoning: resolved.Reasoning, ReviewType: analyze.ReviewType(typ), CustomPrompt: prompt, OutputPrefix: analyze.BuildOutputPrefix(typ, names), AnalysisType: typ, AnalysisFiles: names, AnalysisCommitSHA: head, Source: storage.JobSourceScheduled}})
		if err != nil {
			return err
		}
		if result.Status != 201 {
			return fmt.Errorf("enqueue analysis: %v", result.Body)
		}
	}
	return nil
}
