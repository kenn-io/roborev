package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/storage"
)

type goalWatchInput struct {
	enabled  bool
	watch    []string
	snapshot goalreview.Snapshot
}
type goalCheckout struct {
	root       string
	configRoot string
}
type goalWatchDeps struct {
	checkouts func(context.Context) ([]goalCheckout, error)
	inspect   func(context.Context, goalCheckout) (goalWatchInput, error)
	pending   func(string, []string) ([]string, error)
	enqueue   func(context.Context, goalCheckout, goalreview.Snapshot) (string, error)
	report    func(string, error)
}
type goalWatcher struct {
	deps         goalWatchDeps
	fingerprints map[string]string
}

func newGoalWatcher(deps goalWatchDeps) *goalWatcher {
	return &goalWatcher{deps: deps, fingerprints: map[string]string{}}
}

func (w *goalWatcher) poll(ctx context.Context) {
	checkouts, err := w.deps.checkouts(ctx)
	if err != nil {
		w.deps.report("", err)
		return
	}
	seen := map[string]bool{}
	for _, checkout := range checkouts {
		root := checkout.root
		if ctx.Err() != nil {
			return
		}
		seen[root] = true
		input, err := w.deps.inspect(ctx, checkout)
		if err != nil {
			w.deps.report(root, err)
			continue
		}
		if !input.enabled || len(input.watch) == 0 {
			delete(w.fingerprints, root)
			continue
		}
		hash := input.snapshot.WatchID(input.watch)
		if hash == w.fingerprints[root] {
			continue
		}
		pending, err := w.deps.pending(root, input.watch)
		if err != nil {
			w.deps.report(root, err)
			continue
		}
		if slices.Contains(pending, hash) {
			w.fingerprints[root] = hash
			continue
		}
		if len(pending) > 0 {
			continue
		}
		admitted, err := w.deps.enqueue(ctx, checkout, input.snapshot)
		if err != nil {
			w.deps.report(root, err)
			continue
		}
		w.fingerprints[root] = admitted
	}
	for root := range w.fingerprints {
		if !seen[root] {
			delete(w.fingerprints, root)
		}
	}
}

func (w *goalWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		w.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) goalCheckouts(ctx context.Context) ([]goalCheckout, error) {
	repos, err := s.db.ListRepos()
	if err != nil {
		return nil, err
	}
	checkouts := []goalCheckout{}
	seen := map[string]bool{}
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := os.Stat(repo.RootPath); os.IsNotExist(err) {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "-C", repo.RootPath, "worktree", "list", "--porcelain", "-z")
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.reportGoalWatchError(repo.RootPath, fmt.Errorf("list goal review checkouts: %w", err))
			continue
		}
		for block := range strings.SplitSeq(string(out), "\x00\x00") {
			if slices.Contains(strings.Split(block, "\x00"), "bare") || strings.Contains(block, "\x00prunable") {
				continue
			}
			first, _, _ := strings.Cut(block, "\x00")
			if root, ok := strings.CutPrefix(first, "worktree "); ok {
				if info, err := os.Stat(root); err == nil && info.IsDir() {
					root = filepath.Clean(root)
					if !seen[root] {
						checkouts = append(checkouts, goalCheckout{root: root, configRoot: filepath.Clean(repo.RootPath)})
						seen[root] = true
					}
				}
			}
		}
	}
	slices.SortFunc(checkouts, func(a, b goalCheckout) int {
		return strings.Compare(a.root, b.root)
	})
	return checkouts, nil
}

func (s *Server) goalPending(root string, watch []string) ([]string, error) {
	pending := []string{}
	for _, status := range []string{"queued", "running"} {
		jobs, err := s.db.ListJobs(status, "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview), storage.WithoutPrompt())
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			checkout := job.RepoPath
			if job.WorktreePath != "" {
				checkout = job.WorktreePath
			}
			if filepath.Clean(checkout) == filepath.Clean(root) {
				snapshot, err := goalreview.ParseSnapshot(job.Prompt)
				if err != nil {
					return nil, fmt.Errorf("parse pending goal review %d snapshot: %w", job.ID, err)
				}
				if snapshot.ID() != job.GitRef {
					return nil, fmt.Errorf("pending goal review %d snapshot digest mismatch", job.ID)
				}
				pending = append(pending, snapshot.WatchID(watch))
			}
		}
	}
	return pending, nil
}

func (s *Server) startGoalWatcher(ctx context.Context) {
	s.goalWatchMu.Lock()
	defer s.goalWatchMu.Unlock()
	if s.goalWatchCancel != nil {
		return
	}
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.goalWatchCancel = cancel
	s.goalWatchDone = done
	watcher := newGoalWatcher(goalWatchDeps{
		checkouts: s.goalCheckouts,
		inspect: func(ctx context.Context, checkout goalCheckout) (goalWatchInput, error) {
			repo, err := config.LoadRepoConfig(checkout.configRoot)
			if err != nil {
				return goalWatchInput{}, err
			}
			cfg := config.ResolveGoalReview(repo)
			input := goalWatchInput{enabled: cfg.Enabled, watch: cfg.Watch}
			if !cfg.Enabled || len(cfg.Watch) == 0 {
				return input, nil
			}
			selection, err := goalreview.Select(repo, nil, nil)
			if err != nil {
				return input, err
			}
			input.snapshot, err = goalreview.Capture(ctx, checkout.root, selection, kata.NewCLIClient(checkout.configRoot))
			return input, err
		},
		pending: s.goalPending,
		enqueue: func(ctx context.Context, checkout goalCheckout, _ goalreview.Snapshot) (string, error) {
			output, err := s.enqueueGoalReviewWithConfig(ctx, EnqueueRequest{RepoPath: checkout.root, ReviewType: config.ReviewTypeGoal, Source: "goal_watch"}, checkout.configRoot)
			if err != nil {
				return "", err
			}
			if output.Status != 201 {
				return "", fmt.Errorf("enqueue goal review: %v", output.Body)
			}
			created, ok := output.Body.(EnqueueCreatedResponse)
			if !ok || created.ReviewJob == nil {
				return "", fmt.Errorf("unexpected goal enqueue response")
			}
			snapshot, err := goalreview.ParseSnapshot(created.Prompt)
			if err != nil {
				return "", err
			}
			repo, err := config.LoadRepoConfig(checkout.configRoot)
			if err != nil {
				return "", err
			}
			return snapshot.WatchID(config.ResolveGoalReview(repo).Watch), nil
		},
		report: s.reportGoalWatchError,
	})
	go func() { defer close(done); watcher.run(watchCtx) }()
}

func (s *Server) stopGoalWatcher() {
	s.goalWatchMu.Lock()
	cancel, done := s.goalWatchCancel, s.goalWatchDone
	s.goalWatchCancel = nil
	s.goalWatchDone = nil
	s.goalWatchMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *Server) reportGoalWatchError(root string, err error) {
	log.Printf("goal watcher: %v", err)
	if s.errorLog != nil {
		s.errorLog.LogError("goal_watcher", err.Error(), 0)
	}
	s.broadcaster.Broadcast(Event{Type: "goal_review.watch_error", TS: time.Now(), Repo: root, Error: err.Error()})
}
