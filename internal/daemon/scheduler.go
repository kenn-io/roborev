package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/review/autotype"
	"go.kenn.io/roborev/internal/storage"
)

type analysisScheduler struct {
	server                *Server
	lastRun               map[int64]time.Time
	testHookBeforeEnqueue func()
}

func (s *Server) startAnalysisScheduler(ctx context.Context) {
	scheduler := &analysisScheduler{server: s, lastRun: map[int64]time.Time{}}
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
		if err := w.pollRepo(ctx, repo, global, now); err != nil && ctx.Err() == nil {
			log.Printf("analysis scheduler %s: %v", repo.RootPath, err)
		}
	}
}

func (w *analysisScheduler) pollRepo(ctx context.Context, repo storage.Repo, global *config.Config, now time.Time) error {
	repoCfg, err := config.LoadRepoConfig(repo.RootPath)
	if err != nil {
		return err
	}
	if repoCfg == nil {
		repoCfg = &config.RepoConfig{}
	}
	schedule, err := config.ResolveSchedule(global.Schedule, repoCfg.Schedule)
	if err != nil {
		return err
	}
	if !schedule.Enabled {
		return nil
	}
	return w.runRepo(ctx, repo, repoCfg, global.ForRepo(repo.RootPath), schedule, now)
}

func (w *analysisScheduler) runRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule, now time.Time) error {
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
	if err := w.server.db.SetAnalysisScheduleLastRun(ctx, repo.ID, now); err != nil {
		return err
	}
	w.lastRun[repo.ID] = now
	return w.scheduleRepo(ctx, repo, repoCfg, global, schedule)
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
		return fmt.Errorf("scheduled repository unavailable: %w", err)
	}
	paths, err := git.ListRefFiles(ctx, repo.RootPath, head)
	if err != nil {
		return fmt.Errorf("scheduled repository unavailable: %w", err)
	}
	candidates, err := scheduleCandidates(paths, schedule.Paths)
	if err != nil {
		return err
	}
	usable := make(map[string]bool)
	var errs []error
	for _, typ := range schedule.Types {
		resolved, err := config.ResolveAnalyzeConfigFromConfig("", "", "", repoCfg, global, typ, analyze.Workflow(typ), "")
		if err == nil {
			var workflow agent.WorkflowConfig
			workflow, err = agent.ResolveWorkflowConfigFromConfig(resolved.Agent, repoCfg, global, analyze.Workflow(typ), resolved.Reasoning)
			if err == nil {
				_, err = agent.GetPreferredOrBackupWithConfigFromConfig(repoCfg, workflow.PreferredAgent, global, workflow.BackupAgent)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", typ, err))
			continue
		}
		usable[typ] = true
		if err := w.markDue(ctx, repo, head, typ, candidates); err != nil {
			return err
		}
	}
	selected := selectCandidates(candidates, schedule.MaxFiles)
	for _, typ := range schedule.Types {
		if !usable[typ] {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if names := dueNames(selected, typ); len(names) > 0 {
			if err := w.enqueueType(ctx, repo, typ, names, head); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", typ, err))
			}
		}
	}
	return errors.Join(errs...)
}

func scheduleCandidates(paths, patterns []string) (map[string]*analysisCandidate, error) {
	candidates := map[string]*analysisCandidate{}
	for _, path := range paths {
		matched, err := matchesSchedulePaths(patterns, path)
		if err != nil {
			return nil, err
		}
		if matched {
			candidates[path] = &analysisCandidate{path: path, due: map[string]bool{}}
		}
	}
	return candidates, nil
}

func matchesSchedulePaths(patterns []string, path string) (bool, error) {
	if len(patterns) == 0 {
		return true, nil
	}
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/") {
			pattern += "**"
		}
		ok, err := autotype.AnyMatch([]string{pattern}, path)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// markDue records which candidates need an analysis of the given type.
func (w *analysisScheduler) markDue(ctx context.Context, repo storage.Repo, head, typ string, candidates map[string]*analysisCandidate) error {
	jobs, err := w.server.db.ListJobs("", "", 0, 0, storage.WithRepoIDs([]int64{repo.ID}), storage.WithAnalysisType(typ), storage.WithoutPrompt())
	if err != nil {
		return err
	}
	pending := map[string]bool{}
	baseline := map[string]storage.ReviewJob{}
	attempts := map[string]storage.ReviewJob{}
	for _, job := range jobs {
		for _, path := range job.AnalysisFiles {
			if _, ok := attempts[path]; !ok {
				attempts[path] = job
			}
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
				if changed, err = changedSince(ctx, repo.RootPath, job.AnalysisCommitSHA, head); err != nil {
					return err
				}
				changes[job.AnalysisCommitSHA] = changed
			}
			_, modified := changed.paths[path]
			due = changed.all || modified
		}
		if due {
			candidate.due[typ] = true
			attempt := attempts[path]
			if len(candidate.due) == 1 || attempt.EnqueuedAt.Before(candidate.oldest) {
				candidate.oldest = attempt.EnqueuedAt
			}
		}
	}
	return nil
}

// changedSince lists paths modified between base and head; an unresolvable
// base marks every path changed.
func changedSince(ctx context.Context, repoRoot, base, head string) (changeSet, error) {
	changed := changeSet{paths: map[string]struct{}{}}
	if _, err := git.ResolveCommitCtx(ctx, repoRoot, base); err != nil {
		if ctx.Err() != nil {
			return changed, ctx.Err()
		}
		changed.all = true
		return changed, nil
	}
	paths, err := git.DiffPathsCtx(ctx, repoRoot, base+".."+head, nil)
	for _, p := range paths {
		changed.paths[p] = struct{}{}
	}
	return changed, err
}

func selectCandidates(candidates map[string]*analysisCandidate, limit int) []*analysisCandidate {
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
	if len(selected) > limit {
		selected = selected[:limit]
	}
	return selected
}

func dueNames(selected []*analysisCandidate, typ string) []string {
	var names []string
	for _, c := range selected {
		if c.due[typ] {
			names = append(names, c.path)
		}
	}
	return names
}

// buildTypePrompt reads names at head, honoring ctx cancellation, and builds typ's prompt.
func buildTypePrompt(ctx context.Context, root, typ, head string, names []string) (string, error) {
	files := make(map[string]string, len(names))
	for _, name := range names {
		content, err := git.ReadFile(ctx, root, head, name)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		files[name] = string(content)
	}
	return analyze.GetType(typ).BuildPrompt(files)
}

// lockRegistered coordinates enqueue with repo deletion and merge so it cannot
// recreate a repository removed after it was selected.
func (w *analysisScheduler) lockRegistered(repo storage.Repo) (func(), error) {
	w.server.scheduledRepoMu.Lock()
	if registered, err := w.server.db.GetRepoByID(repo.ID); err != nil || registered.RootPath != repo.RootPath {
		w.server.scheduledRepoMu.Unlock()
		return nil, fmt.Errorf("scheduled repository no longer registered")
	}
	return w.server.scheduledRepoMu.Unlock, nil
}

func (w *analysisScheduler) enqueueType(ctx context.Context, repo storage.Repo, typ string, names []string, head string) error {
	prompt, err := buildTypePrompt(ctx, repo.RootPath, typ, head, names)
	if err != nil {
		return err
	}
	unlock, err := w.lockRegistered(repo)
	if err != nil {
		return err
	}
	defer unlock()
	if w.testHookBeforeEnqueue != nil {
		w.testHookBeforeEnqueue()
	}
	result, err := w.server.humaEnqueue(ctx, &EnqueueInput{Body: EnqueueRequest{RepoPath: repo.RootPath, GitRef: typ, ReviewType: analyze.ReviewType(typ), CustomPrompt: prompt, OutputPrefix: analyze.BuildOutputPrefix(typ, names), AnalysisType: typ, AnalysisFiles: names, AnalysisCommitSHA: head, Source: storage.JobSourceScheduled}})
	if err != nil {
		return err
	}
	if result.Status != 201 {
		return fmt.Errorf("enqueue analysis: %v", result.Body)
	}
	return nil
}
