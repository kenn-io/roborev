package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	retry, err := w.pruneRetry(ctx, repo.ID, schedule.Types)
	if err != nil || !schedule.Enabled {
		return err
	}
	return w.runRepo(ctx, repo, repoCfg, global.ForRepo(repo.RootPath), schedule, retry, now)
}

func (w *analysisScheduler) runRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule, retry []string, now time.Time) error {
	last, loaded := w.lastRun[repo.ID]
	if !loaded {
		var err error
		last, err = w.server.db.AnalysisScheduleLastRun(ctx, repo.ID)
		if err != nil {
			return err
		}
		w.lastRun[repo.ID] = last
	}
	retrying := !last.IsZero() && now.Sub(last) < schedule.Interval
	if retrying {
		var err error
		if schedule.Types, err = w.retryTypes(ctx, repo.ID, last, schedule.Types, retry); err != nil || len(schedule.Types) == 0 {
			return err
		}
	}
	failed, err := w.scheduleRepo(ctx, repo, repoCfg, global, schedule)
	if failed == nil {
		failed = schedule.Types
	}
	err = errors.Join(err, w.server.db.SetAnalysisScheduleRetry(ctx, repo.ID, failed))
	if retrying {
		return err
	}
	if setErr := w.server.db.SetAnalysisScheduleLastRun(ctx, repo.ID, now); setErr != nil {
		return errors.Join(err, setErr)
	}
	w.lastRun[repo.ID] = now
	return err
}

// pruneRetry drops saved retries for types outside the schedule and returns
// the rest. A disabled schedule has no types, so its retries are all dropped.
func (w *analysisScheduler) pruneRetry(ctx context.Context, repoID int64, types []string) ([]string, error) {
	saved, err := w.server.db.AnalysisScheduleRetry(ctx, repoID)
	if err != nil {
		return nil, err
	}
	kept := slices.DeleteFunc(slices.Clone(saved), func(typ string) bool { return !slices.Contains(types, typ) })
	if len(kept) == len(saved) {
		return kept, nil
	}
	return kept, w.server.db.SetAnalysisScheduleRetry(ctx, repoID, kept)
}

// retryTypes returns the configured types that failed to enqueue, or whose
// latest job from the current pass failed.
func (w *analysisScheduler) retryTypes(ctx context.Context, repoID int64, last time.Time, types, saved []string) ([]string, error) {
	var retry []string
	for _, typ := range types {
		jobs, err := w.server.db.ListJobs("", "", 1, 0, storage.WithRepoIDs([]int64{repoID}), storage.WithAnalysisType(typ), storage.WithSource(storage.JobSourceScheduled), storage.WithoutPrompt())
		if err != nil {
			return nil, err
		}
		failedJob := len(jobs) > 0 && jobs[0].Status == storage.JobStatusFailed && jobs[0].FinishedAt != nil && jobs[0].FinishedAt.Add(time.Second).After(last)
		if failedJob || slices.Contains(saved, typ) {
			retry = append(retry, typ)
		}
	}
	return retry, nil
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

// scheduleRepo queues due files and returns the types that failed. It returns
// nil types when it fails before queueing, so every type is retried.
func (w *analysisScheduler) scheduleRepo(ctx context.Context, repo storage.Repo, repoCfg *config.RepoConfig, global *config.Config, schedule config.Schedule) ([]string, error) {
	head, err := git.ResolveCommitCtx(ctx, repo.RootPath, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("scheduled repository unavailable: %w", err)
	}
	tree, err := git.ListRefFiles(ctx, repo.RootPath, head)
	if err != nil {
		return nil, fmt.Errorf("scheduled repository unavailable: %w", err)
	}
	candidates, err := scheduleCandidates(tree, schedule.Paths)
	if err != nil {
		return nil, err
	}
	failed := []string{}
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
			failed = append(failed, typ)
			errs = append(errs, fmt.Errorf("%s: %w", typ, err))
			continue
		}
		usable[typ] = true
		if err := w.markDue(ctx, repo, head, typ, candidates); err != nil {
			return nil, err
		}
	}
	ordered := selectCandidates(candidates)
	if len(ordered) == 0 {
		return failed, errors.Join(errs...)
	}
	var attributes []git.TreeBlob
	var oids, names []string
	for _, blob := range tree {
		if path.Base(blob.Path) == ".gitattributes" {
			attributes = append(attributes, blob)
			oids = append(oids, blob.OID)
		}
	}
	for _, candidate := range ordered {
		oids, names = append(oids, candidate.oid), append(names, candidate.path)
	}
	missing, large, err := git.CheckBlobs(ctx, repo.RootPath, oids, config.ResolveMaxPromptSize(repo.RootPath, global)+1)
	if err != nil {
		return nil, err
	}
	binary, err := git.BinaryAttributes(ctx, repo.RootPath, head, names)
	if err != nil {
		return nil, err
	}
	reader, err := git.NewBlobReader(ctx, repo.RootPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var selected []*analysisCandidate
	files := map[string]string{}
	var skipped []string
	for _, candidate := range ordered {
		if len(selected) == schedule.MaxFiles {
			break
		}
		if missing[candidate.oid] || attributesMissing(attributes, missing, candidate.path) {
			continue
		}
		if binary[candidate.path] || large[candidate.oid] {
			skipped = append(skipped, candidate.path)
			continue
		}
		content, err := reader.Read(candidate.oid)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			errs = append(errs, err)
			continue
		}
		if !isSourceContent(content) {
			skipped = append(skipped, candidate.path)
			continue
		}
		files[candidate.path] = string(content)
		selected = append(selected, candidate)
	}
	if len(skipped) > 0 {
		log.Printf("analysis scheduler %s: skipped %d binary or oversized files: %s", repo.RootPath, len(skipped), strings.Join(skipped[:min(len(skipped), 3)], ", "))
	}
	for _, typ := range schedule.Types {
		if !usable[typ] {
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if names := dueNames(selected, typ); len(names) > 0 {
			if err := w.enqueueType(ctx, repo, typ, names, head, files); err != nil {
				failed = append(failed, typ)
				errs = append(errs, fmt.Errorf("%s: %w", typ, err))
			}
		}
	}
	return failed, errors.Join(errs...)
}

// attributesMissing reports whether a committed .gitattributes file that can
// apply to p is unavailable locally, leaving p's classification unknown.
func attributesMissing(attributes []git.TreeBlob, missing map[string]bool, p string) bool {
	for _, file := range attributes {
		if dir := path.Dir(file.Path); missing[file.OID] && (dir == "." || strings.HasPrefix(p, dir+"/")) {
			return true
		}
	}
	return false
}

func scheduleCandidates(tree []git.TreeBlob, patterns []string) (map[string]*analysisCandidate, error) {
	candidates := map[string]*analysisCandidate{}
	for _, blob := range tree {
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

// isSourceContent accepts non-NUL source text, including legacy encodings such
// as Latin-1, and rejects content with a binary signature such as PDF or with
// no valid UTF-8 at all.
func isSourceContent(content []byte) bool {
	kind := http.DetectContentType(content)
	return isSourceText(string(content)) &&
		(strings.HasPrefix(kind, "text/") || kind == "application/octet-stream" && utf8.Valid(content))
}

func isSourceText(text string) bool {
	return !strings.ContainsRune(text, 0) &&
		(text == "" || strings.ToValidUTF8(text, "") != "")
}
