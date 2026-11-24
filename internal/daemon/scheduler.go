package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
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
	failedTypes           map[int64]map[string]bool
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
		delete(w.failedTypes, repo.ID)
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
	if w.failedTypes == nil {
		w.failedTypes = map[int64]map[string]bool{}
	}
	retry := w.failedTypes[repo.ID]
	configured := map[string]bool{}
	for _, typ := range schedule.Types {
		configured[typ] = true
	}
	for typ := range retry {
		if !configured[typ] {
			delete(retry, typ)
		}
	}
	if !last.IsZero() && now.Sub(last) < schedule.Interval {
		if len(retry) == 0 {
			return nil
		}
		var types []string
		for _, typ := range schedule.Types {
			if retry[typ] {
				types = append(types, typ)
			}
		}
		schedule.Types = types
	}
	retrying := !last.IsZero() && now.Sub(last) < schedule.Interval
	failed, scheduleErr := w.scheduleRepoTypes(ctx, repo, repoCfg, global, schedule)
	w.failedTypes[repo.ID] = failed
	if retrying {
		return scheduleErr
	}
	if err := w.server.db.SetAnalysisScheduleLastRun(ctx, repo.ID, now); err != nil {
		return errors.Join(scheduleErr, err)
	}
	w.lastRun[repo.ID] = now
	return scheduleErr
}

type changeSet struct {
	all   bool
	paths map[string]struct{}
}

type analysisCandidate struct {
	path   string
	oid    string
	due    map[string]bool
	oldest time.Time
}

func (w *analysisScheduler) scheduleRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule) error {
	_, err := w.scheduleRepoTypes(ctx, repo, repoCfg, global, schedule)
	return err
}

func (w *analysisScheduler) scheduleRepoTypes(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule) (failed map[string]bool, result error) {
	failed = map[string]bool{}
	setupFailed := true
	defer func() {
		if setupFailed && result != nil {
			for _, typ := range schedule.Types {
				failed[typ] = true
			}
		}
	}()
	head, err := git.ResolveCommitCtx(ctx, repo.RootPath, "HEAD")
	if err != nil {
		return failed, fmt.Errorf("scheduled repository unavailable: %w", err)
	}
	paths, err := git.ListRefFiles(ctx, repo.RootPath, head)
	if err != nil {
		return failed, fmt.Errorf("scheduled repository unavailable: %w", err)
	}
	candidates, err := scheduleCandidates(paths, schedule.Paths)
	if err != nil {
		return failed, err
	}
	budget := config.ResolveMaxPromptSize(repo.RootPath, global)
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
			failed[typ] = true
			errs = append(errs, fmt.Errorf("%s: %w", typ, err))
			continue
		}
		usable[typ] = true
		if err := w.markDue(ctx, repo, head, typ, candidates); err != nil {
			return failed, err
		}
	}
	ordered := selectCandidates(candidates)
	if len(ordered) == 0 {
		setupFailed = false
		return failed, errors.Join(errs...)
	}
	blobs := make([]git.TreeBlob, 0, len(ordered))
	for _, candidate := range ordered {
		blobs = append(blobs, git.TreeBlob{Path: candidate.path, OID: candidate.oid})
	}
	missing, err := git.MissingTreeBlobs(ctx, repo.RootPath, blobs)
	if err != nil {
		return failed, err
	}
	pathsForAttributes := make([]string, len(ordered))
	for i, candidate := range ordered {
		pathsForAttributes[i] = candidate.path
	}
	binary, err := git.BinaryAttributes(ctx, repo.RootPath, head, pathsForAttributes)
	if err != nil {
		return failed, err
	}
	reader, err := git.NewTreeBlobReader(ctx, repo.RootPath)
	if err != nil {
		return failed, err
	}
	defer func() {
		if reader != nil {
			_ = reader.Close()
		}
	}()
	var selected []*analysisCandidate
	files := map[string]string{}
	var skipped []string
	for _, candidate := range ordered {
		if len(selected) == schedule.MaxFiles {
			break
		}
		if missing[candidate.oid] {
			continue
		}
		if binary[candidate.path] {
			skipped = append(skipped, candidate.path)
			continue
		}
		size, err := reader.Size(candidate.oid)
		if err != nil {
			if ctx.Err() != nil {
				return failed, ctx.Err()
			}
			for typ := range candidate.due {
				failed[typ] = true
			}
			errs = append(errs, fmt.Errorf("size %s: %w", candidate.path, err))
			_ = reader.Close()
			reader, err = git.NewTreeBlobReader(ctx, repo.RootPath)
			if err != nil {
				return failed, errors.Join(errors.Join(errs...), err)
			}
			continue
		}
		if size > int64(budget) {
			skipped = append(skipped, candidate.path)
			continue
		}
		content, err := reader.Read(candidate.oid)
		if err != nil {
			if ctx.Err() != nil {
				return failed, ctx.Err()
			}
			for typ := range candidate.due {
				failed[typ] = true
			}
			errs = append(errs, fmt.Errorf("read %s: %w", candidate.path, err))
			_ = reader.Close()
			reader, err = git.NewTreeBlobReader(ctx, repo.RootPath)
			if err != nil {
				return failed, errors.Join(errors.Join(errs...), err)
			}
			continue
		}
		if analyze.IsBinary(string(content)) || !strings.HasPrefix(http.DetectContentType(content), "text/") {
			skipped = append(skipped, candidate.path)
			continue
		}
		files[candidate.path] = string(content)

		selected = append(selected, candidate)
	}
	if len(skipped) > 0 {
		log.Printf("analysis scheduler %s: skipped %d binary or oversized files: %s", repo.RootPath, len(skipped), strings.Join(skipped[:min(len(skipped), 3)], ", "))
	}
	setupFailed = false
	for _, typ := range schedule.Types {
		if !usable[typ] {
			continue
		}
		if ctx.Err() != nil {
			return failed, ctx.Err()
		}
		if names := dueNames(selected, typ); len(names) > 0 {
			if err := w.enqueueType(ctx, repo, typ, names, head, files); err != nil {
				failed[typ] = true
				errs = append(errs, fmt.Errorf("%s: %w", typ, err))
			}
		}
	}
	return failed, errors.Join(errs...)
}

func scheduleCandidates(paths []git.TreeBlob, patterns []string) (map[string]*analysisCandidate, error) {
	candidates := map[string]*analysisCandidate{}
	for _, blob := range paths {
		matched, err := matchesSchedulePaths(patterns, blob.Path)
		if err != nil {
			return nil, err
		}
		if matched && analyze.IsCodeFile(blob.Path) {
			candidates[blob.Path] = &analysisCandidate{path: blob.Path, oid: blob.OID, due: map[string]bool{}}
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

func selectCandidates(candidates map[string]*analysisCandidate) []*analysisCandidate {
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

// lockRegistered coordinates enqueue with repo deletion, merge, and move so it
// cannot recreate a repository removed or moved after it was selected.
func (w *analysisScheduler) lockRegistered(repo storage.Repo) (func(), error) {
	w.server.scheduledRepoMu.Lock()
	if registered, err := w.server.db.GetRepoByID(repo.ID); err != nil || registered.RootPath != repo.RootPath {
		w.server.scheduledRepoMu.Unlock()
		return nil, fmt.Errorf("scheduled repository no longer registered")
	}
	return w.server.scheduledRepoMu.Unlock, nil
}

func (w *analysisScheduler) enqueueType(ctx context.Context, repo storage.Repo, typ string, names []string, head string, contents map[string]string) error {
	files := make(map[string]string, len(names))
	for _, name := range names {
		files[name] = contents[name]
	}
	prompt, err := analyze.GetType(typ).BuildPrompt(files)
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
