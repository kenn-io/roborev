package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
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
	server   *Server
	lastRun  map[int64]time.Time
	failedAt map[int64]time.Time
	cleared  map[int64]struct{} // repos whose persisted last-run state is known absent
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

// persistentError marks a failure that retrying cannot fix until config,
// repository state, or the daemon changes.
type persistentError struct{ error }

func (e persistentError) Unwrap() error { return e.error }

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
	repoCfg, err := loadScheduledRepoConfig(repo.RootPath)
	if err != nil {
		return err
	}
	schedule, err := config.ResolveSchedule(global.Schedule, repoCfg.Schedule)
	if err != nil {
		return err
	}
	if !schedule.Enabled {
		return w.clearRepo(ctx, repo.ID)
	}
	return w.runRepo(ctx, repo, repoCfg, global.ForRepo(repo.RootPath), schedule, now)
}

// loadScheduledRepoConfig skips config lookup, including Git-root discovery,
// for paths that have neither a local config nor a .git entry.
func loadScheduledRepoConfig(root string) (*config.RepoConfig, error) {
	if !hasRepoMarker(root) {
		return &config.RepoConfig{}, nil
	}
	cfg, err := config.LoadRepoConfig(root)
	if err == nil && cfg == nil {
		cfg = &config.RepoConfig{}
	}
	return cfg, err
}

func hasRepoMarker(root string) bool {
	for _, name := range []string{".roborev.toml", ".git"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

func (w *analysisScheduler) clearRepo(ctx context.Context, repoID int64) error {
	delete(w.failedAt, repoID)
	return w.clearLastRun(ctx, repoID)
}

// clearLastRun clears persisted last-run state once until the repo is enabled again.
func (w *analysisScheduler) clearLastRun(ctx context.Context, repoID int64) error {
	if _, ok := w.cleared[repoID]; ok {
		return nil
	}
	if err := w.server.db.ClearAnalysisScheduleLastRun(ctx, repoID); err != nil {
		return err
	}
	delete(w.lastRun, repoID)
	if w.cleared == nil {
		w.cleared = make(map[int64]struct{})
	}
	w.cleared[repoID] = struct{}{}
	return nil
}

// runRepo backs off persistent failures so they neither log nor retry on every
// poll; transient failures retry on the next one.
func (w *analysisScheduler) runRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule, now time.Time) error {
	delete(w.cleared, repo.ID)
	if failed, ok := w.failedAt[repo.ID]; ok && now.Sub(failed) < min(5*time.Minute, schedule.Interval) {
		return nil
	}
	err := w.runDue(ctx, repo, repoCfg, global, schedule, now)
	w.recordRun(ctx, repo.ID, err, now)
	return err
}

func (w *analysisScheduler) recordRun(ctx context.Context, repoID int64, err error, now time.Time) {
	if err == nil {
		delete(w.failedAt, repoID)
	} else if ctx.Err() == nil && errors.As(err, new(persistentError)) {
		if w.failedAt == nil {
			w.failedAt = make(map[int64]time.Time)
		}
		w.failedAt[repoID] = now
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
		return persistentError{fmt.Errorf("scheduled repository unavailable: %w", err)}
	}
	paths, err := git.ListRefFiles(ctx, repo.RootPath, head)
	if err != nil {
		return persistentError{fmt.Errorf("scheduled repository unavailable: %w", err)}
	}
	candidates, err := scheduleCandidates(paths, schedule.Paths)
	if err != nil {
		return err
	}
	for _, typ := range schedule.Types {
		if err := w.markDue(ctx, repo, head, typ, candidates); err != nil {
			return err
		}
	}
	selected := selectCandidates(candidates, schedule.MaxFiles)
	var errs []error
	for _, typ := range schedule.Types {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if names := dueNames(selected, typ); len(names) > 0 {
			if err := w.enqueueType(ctx, repo, repoCfg, global, typ, names, head); err != nil {
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
			if !ok {
				candidate.oldest = time.Time{}
			} else if len(candidate.due) == 1 || (!candidate.oldest.IsZero() && job.EnqueuedAt.Before(candidate.oldest)) {
				candidate.oldest = job.EnqueuedAt
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

func (w *analysisScheduler) enqueueType(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, typ string, names []string, head string) error {
	resolved, err := config.ResolveAnalyzeConfigFromConfig("", "", "", repoCfg, global, typ, analyze.Workflow(typ), "")
	if err != nil {
		return persistentError{err}
	}
	prompt, err := analyze.GetType(typ).BuildPromptWithPaths(".", names)
	if err != nil {
		return persistentError{err}
	}
	result, err := w.server.humaEnqueue(ctx, &EnqueueInput{Body: EnqueueRequest{RepoPath: repo.RootPath, GitRef: typ, Agent: resolved.Agent, Model: resolved.Model, Reasoning: resolved.Reasoning, ReviewType: analyze.ReviewType(typ), CustomPrompt: prompt, OutputPrefix: analyze.BuildOutputPrefix(typ, names), AnalysisType: typ, AnalysisFiles: names, AnalysisCommitSHA: head, Source: storage.JobSourceScheduled}})
	if err != nil {
		return err
	}
	if result.Status != 201 {
		err := fmt.Errorf("enqueue analysis: %v", result.Body)
		if result.Status < 500 {
			return persistentError{err}
		}
		return err
	}
	return nil
}
