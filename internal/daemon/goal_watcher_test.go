package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func canonicalGoalCheckoutPaths(t *testing.T, paths []string) []string {
	t.Helper()
	canonical := make([]string, len(paths))
	for i, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)
		canonical[i] = resolved
	}
	return canonical
}

func goalCheckoutPaths(checkouts []goalCheckout) []string {
	paths := make([]string, len(checkouts))
	for i, checkout := range checkouts {
		paths[i] = checkout.root
	}
	return paths
}

func TestGoalWatchCoalescesAndRetries(t *testing.T) {
	root := "checkout"
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	pending := false
	var pendingHash string
	admitted := 0
	enqueueErr := errors.New("queue unavailable")
	captureErr := error(nil)
	enabled := true
	var reported []error
	watcher := newGoalWatcher(goalWatchDeps{
		checkouts: func(context.Context) ([]goalCheckout, error) {
			return []goalCheckout{{root: root, configRoot: root}}, nil
		},
		inspect: func(context.Context, goalCheckout) (goalWatchInput, error) {
			return goalWatchInput{enabled: enabled, watch: []string{"goal", "kata_graph"}, snapshot: snapshot}, captureErr
		},
		completed: func(string, []string) (string, error) { return "", nil },
		pending: func(string, []string) ([]string, error) {
			if pending {
				return []string{pendingHash}, nil
			}
			return nil, nil
		},
		enqueue: func(_ context.Context, _ goalCheckout, admittedSnapshot goalreview.Snapshot) (string, error) {
			if enqueueErr != nil {
				return "", enqueueErr
			}
			admitted++
			pending = true
			pendingHash = admittedSnapshot.WatchID([]string{"goal", "kata_graph"})
			return pendingHash, nil
		},
		report: func(_ string, err error) { reported = append(reported, err) },
	})
	watcher.poll(context.Background())
	assert.Zero(t, admitted)
	require.Len(t, reported, 1)
	enqueueErr = nil
	watcher.poll(context.Background())
	assert.Equal(t, 1, admitted)
	snapshot.Artifacts[0].Content += "More requirements\n"
	watcher.poll(context.Background())
	assert.Equal(t, 1, admitted)
	pending = false
	watcher.poll(context.Background())
	assert.Equal(t, 2, admitted)
	pending = false
	captureErr = errors.New("ambiguous history")
	watcher.poll(context.Background())
	assert.Equal(t, 2, admitted)
	captureErr = nil
	watcher.poll(context.Background())
	assert.Equal(t, 2, admitted)
	enabled = false
	watcher.poll(context.Background())
	assert.Empty(t, watcher.fingerprints)
}

func TestGoalWatchDoesNotEnqueueSnapshotAlreadyPendingAtStartup(t *testing.T) {
	root := "checkout"
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	pending := true
	pendingHash := snapshot.WatchID([]string{"goal"})
	enqueued := 0
	watcher := newGoalWatcher(goalWatchDeps{
		checkouts: func(context.Context) ([]goalCheckout, error) {
			return []goalCheckout{{root: root, configRoot: root}}, nil
		},
		inspect: func(context.Context, goalCheckout) (goalWatchInput, error) {
			return goalWatchInput{enabled: true, watch: []string{"goal"}, snapshot: snapshot}, nil
		},
		completed: func(string, []string) (string, error) { return "", nil },
		pending: func(string, []string) ([]string, error) {
			if pending {
				return []string{pendingHash}, nil
			}
			return nil, nil
		},
		enqueue: func(_ context.Context, _ goalCheckout, enqueuedSnapshot goalreview.Snapshot) (string, error) {
			enqueued++
			return enqueuedSnapshot.WatchID([]string{"goal"}), nil
		},
		report: func(string, error) {},
	})

	watcher.poll(context.Background())
	assert.Zero(t, enqueued)
	snapshot.Artifacts[0].Content += "Changed while the first review is pending\n"
	watcher.poll(context.Background())
	assert.Zero(t, enqueued, "a changed snapshot waits for the pending review")
	pending = false
	watcher.poll(context.Background())
	assert.Equal(t, 1, enqueued, "the changed snapshot should be reviewed after the pending one finishes")
}

func TestGoalWatchDoesNotRepeatCompletedSnapshotAfterRestart(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	deps := goalWatchDeps{
		checkouts: func(context.Context) ([]goalCheckout, error) {
			return []goalCheckout{{root: repo.Path(), configRoot: repo.Path()}}, nil
		},
		inspect: func(context.Context, goalCheckout) (goalWatchInput, error) {
			return goalWatchInput{enabled: true, watch: []string{"goal"}, snapshot: snapshot}, nil
		},
		pending:   server.goalPending,
		completed: server.goalCompleted,
		enqueue: func(_ context.Context, _ goalCheckout, snapshot goalreview.Snapshot) (string, error) {
			_, err := db.EnqueueJob(storage.EnqueueOpts{
				RepoID: registered.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: "goal",
				JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true,
			})
			return snapshot.WatchID([]string{"goal"}), err
		},
		report: func(_ string, err error) { require.NoError(t, err) },
	}
	newGoalWatcher(deps).poll(context.Background())
	job, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "pi", job.Prompt, "No issues found."))

	watcher := newGoalWatcher(deps)
	watcher.poll(context.Background())
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	assert.Len(t, jobs, 1, "restarting must not review unchanged completed evidence again")
	snapshot.Artifacts[0].Content += "New requirement\n"
	watcher.poll(context.Background())
	jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, snapshot.ID(), jobs[0].GitRef, "changed evidence still needs review")
}

func TestGoalWatchReportsErrorAgainOnlyAfterRecovery(t *testing.T) {
	for _, stage := range []string{"checkouts", "inspect", "pending", "enqueue"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New("service unavailable")
			var reported []error
			deps := goalWatchDeps{
				checkouts: func(context.Context) ([]goalCheckout, error) {
					if stage == "checkouts" && failure != nil {
						return nil, failure
					}
					return []goalCheckout{{root: "checkout"}}, nil
				},
				inspect: func(context.Context, goalCheckout) (goalWatchInput, error) {
					input := goalWatchInput{enabled: true, watch: []string{"goal"}, snapshot: goalreview.Snapshot{Source: "superpowers"}}
					if stage == "inspect" {
						return input, failure
					}
					return input, nil
				},
				completed: func(string, []string) (string, error) { return "", nil },
				pending: func(string, []string) ([]string, error) {
					if stage == "pending" {
						return nil, failure
					}
					return nil, nil
				},
				enqueue: func(context.Context, goalCheckout, goalreview.Snapshot) (string, error) {
					if stage == "enqueue" {
						return "", failure
					}
					return "", nil
				},
				report: func(_ string, err error) { reported = append(reported, err) },
			}
			watcher := newGoalWatcher(deps)
			watcher.poll(context.Background())
			watcher.poll(context.Background())
			assert.Len(t, reported, 1)
			failure = nil
			watcher.poll(context.Background())
			failure = errors.New("service unavailable")
			watcher.poll(context.Background())
			watcher.poll(context.Background())
			assert.Len(t, reported, 2)
		})
	}
}

func TestGoalWatchSkipsDiscoveryWhenDisabled(t *testing.T) {
	for _, contents := range []string{"", "[goal_review]\nenabled = false\nwatch = [\"goal\"]\n", "[goal_review]\nenabled = true\nwatch = []\n"} {
		t.Run(contents, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, ".roborev.toml"), []byte(contents), 0o600))
			_, err := db.GetOrCreateRepo(root)
			require.NoError(t, err)
			roots, err := server.goalCheckouts(context.Background())
			require.NoError(t, err)
			assert.Empty(t, roots)
			assert.Empty(t, server.errorLog.Recent(), "disabled repositories must not run git discovery")
		})
	}
}

func TestGoalWatchWaitsForAutoDiscoveredSpec(t *testing.T) {
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	repo.CommitFile(".roborev.toml", "review_agent = \"pi\"\n[goal_review]\nenabled = true\nwatch = [\"goal\"]\n", "configure watcher")
	_, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	watcher := server.newGoalWatcher()
	watcher.poll(context.Background())
	assert.Empty(t, server.errorLog.Recent(), "no auto-discovered spec means the watcher is idle")
	jobs, err := db.ListJobs("", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	assert.Empty(t, jobs)
	manual, err := server.enqueueGoalReview(context.Background(), EnqueueRequest{RepoPath: repo.Path()})
	require.NoError(t, err)
	assert.Equal(t, 400, manual.Status, "manual review still needs a spec")
	goalArtifacts(t, repo.Path())
	watcher.poll(context.Background())
	jobs, err = db.ListJobs("queued", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
}

func TestGoalWatchEnqueuesInspectedSnapshot(t *testing.T) {
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	repo.CommitFile(".roborev.toml", "review_agent = \"pi\"\n[goal_review]\nenabled = true\nwatch = [\"goal\"]\n", "configure watcher")
	goalArtifacts(t, repo.Path())
	_, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	watcher := server.newGoalWatcher()
	enqueue := watcher.deps.enqueue
	watcher.deps.enqueue = func(ctx context.Context, checkout goalCheckout, snapshot goalreview.Snapshot) (string, error) {
		require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), "docs/superpowers/specs/feature-design.md"), []byte("# Changed after inspection\n"), 0o600))
		return enqueue(ctx, checkout, snapshot)
	}
	watcher.poll(context.Background())
	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	snapshot, err := goalreview.ParseSnapshot(jobs[0].Prompt)
	require.NoError(t, err)
	require.Contains(t, snapshot.Artifacts, goalreview.Artifact{Kind: "spec", Path: "docs/superpowers/specs/feature-design.md", Content: "# Feature\n"})
}

func TestGoalPendingReturnsFrozenSnapshotWatchID(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Frozen\n"}}}
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID: registered.ID, Agent: "pi", GitRef: snapshot.ID(), ReviewType: "goal",
		JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true,
	})
	require.NoError(t, err)
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID: registered.ID, Agent: "pi", GitRef: "hypothetical", ReviewType: "goal",
		JobType: storage.JobTypeGoalReview, Prompt: "Candidate evidence", PromptPrebuilt: true, Source: "goal_gate",
	})
	require.NoError(t, err)

	pending, err := server.goalPending(repo.Path(), []string{"goal"})
	require.NoError(t, err)
	assert.Equal(t, []string{snapshot.WatchID([]string{"goal"})}, pending)
}

func TestGoalWatchInspectUsesCallerContextWithoutAddingDeadline(t *testing.T) {
	deadlineAdded := false
	watcher := newGoalWatcher(goalWatchDeps{
		checkouts: func(context.Context) ([]goalCheckout, error) {
			return []goalCheckout{{root: "checkout"}}, nil
		},
		inspect: func(ctx context.Context, _ goalCheckout) (goalWatchInput, error) {
			_, deadlineAdded = ctx.Deadline()
			return goalWatchInput{}, nil
		},
		completed: func(string, []string) (string, error) { return "", nil },
		pending:   func(string, []string) ([]string, error) { return nil, nil },
		enqueue: func(context.Context, goalCheckout, goalreview.Snapshot) (string, error) {
			return "", nil
		},
		report: func(string, error) {},
	})

	watcher.poll(context.Background())
	assert.False(t, deadlineAdded, "inspection should run until the watcher is canceled")
}

func TestGoalWatchDiscoversWorktreesDespiteInvalidRepo(t *testing.T) {
	server, db, _ := newTestServer(t)
	invalid := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(invalid, ".roborev.toml"), []byte("[goal_review]\nenabled = true\nwatch = [\"goal\"]\n"), 0o600))
	_, err := db.GetOrCreateRepo(invalid)
	require.NoError(t, err)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile(".roborev.toml", "[goal_review]\nenabled = true\nwatch = [\"goal\"]\n", "configure watcher")
	_, err = db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	repo.RunGit("commit", "--allow-empty", "-m", "Initial test commit")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.RunGit("worktree", "add", "--detach", linked)
	roots, err := server.goalCheckouts(context.Background())
	require.NoError(t, err)
	var discovered []string
	for _, checkout := range roots {
		if checkout.err == nil {
			discovered = append(discovered, checkout.root)
		}
	}
	assert.ElementsMatch(t,
		canonicalGoalCheckoutPaths(t, []string{repo.Path(), linked}),
		canonicalGoalCheckoutPaths(t, discovered),
	)
	watcher := server.newGoalWatcher()
	watcher.poll(context.Background())
	watcher.poll(context.Background())
	require.Len(t, server.errorLog.Recent(), 1, "a persistent git discovery error should be reported once")
	assert.Contains(t, server.errorLog.Recent()[0].Message, "list goal review checkouts")
}

func TestGoalWatchUsesRegisteredCheckoutConfig(t *testing.T) {
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	repo.CommitFile(".roborev.toml", "review_agent = \"pi\"\n[goal_review]\nenabled = true\nwatch = [\"goal\"]\n", "configure automatic goal review")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.RunGit("worktree", "add", "--detach", linked)
	goalArtifacts(t, repo.Path())
	goalArtifacts(t, linked)
	linkedSpec := filepath.Join(linked, "docs/superpowers/specs/feature-design.md")
	require.NoError(t, os.WriteFile(linkedSpec, []byte("# Linked checkout feature\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(linked, ".roborev.toml"),
		[]byte("review_agent = \"untrusted-agent\"\n[goal_review]\nenabled = false\nwatch = [\"goal\"]\n"), 0o600))
	_, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)

	subscriberID, events := server.broadcaster.Subscribe("")
	defer server.broadcaster.Unsubscribe(subscriberID)
	ctx, cancel := context.WithCancel(context.Background())
	server.startGoalWatcher(ctx)
	t.Cleanup(func() {
		cancel()
		server.stopGoalWatcher()
	})

	rootQueued, linkedQueued := false, false
	var watchErr error
	timedOut := false
	// The initial poll launches git and Kata subprocesses before persisting each job.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for (!rootQueued || !linkedQueued) && watchErr == nil && !timedOut {
		select {
		case event := <-events:
			if event.Type == "goal_review.watch_error" {
				watchErr = errors.New(event.Error)
				continue
			}
			if event.Type != "goal_review.enqueued" {
				continue
			}
			if event.WorktreePath == "" {
				rootQueued = true
				continue
			}
			resolved, err := filepath.EvalSymlinks(event.WorktreePath)
			require.NoError(t, err)
			linkedResolved, err := filepath.EvalSymlinks(linked)
			require.NoError(t, err)
			linkedQueued = resolved == linkedResolved
		case <-timer.C:
			timedOut = true
		}
	}
	require.NoError(t, watchErr, "watcher must ignore linked-checkout config")
	assert.False(t, timedOut)
	assert.True(t, rootQueued)
	assert.True(t, linkedQueued)

	jobs, err := db.ListJobs("queued", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	for _, job := range jobs {
		assert.Equal(t, "pi", job.Agent)
	}
}

func TestGoalWatchStopJoinsActivePoll(t *testing.T) {
	server, _, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	server.startGoalWatcher(ctx)
	server.goalWatchMu.Lock()
	done := server.goalWatchDone
	server.goalWatchMu.Unlock()
	cancel()
	stopped := make(chan struct{})
	go func() { server.stopGoalWatcher(); close(stopped) }()
	require.Eventually(t, func() bool {
		select {
		case <-stopped:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond)
	joined := false
	select {
	case <-done:
		joined = true
	default:
	}
	assert.True(t, joined, "stop must join the polling goroutine")
	assert.Nil(t, server.goalWatchCancel)
}

func TestGoalWatchStagesAndCheckboxes(t *testing.T) {
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	roots := []goalCheckout{{root: "main", configRoot: "main"}, {root: "linked", configRoot: "main"}}
	calls := map[string]int{}
	watcher := newGoalWatcher(goalWatchDeps{
		checkouts: func(context.Context) ([]goalCheckout, error) { return roots, nil },
		inspect: func(context.Context, goalCheckout) (goalWatchInput, error) {
			return goalWatchInput{enabled: true, watch: []string{"goal"}, snapshot: snapshot}, nil
		},
		completed: func(string, []string) (string, error) { return "", nil },
		pending:   func(string, []string) ([]string, error) { return nil, nil },
		enqueue: func(_ context.Context, checkout goalCheckout, s goalreview.Snapshot) (string, error) {
			calls[checkout.root]++
			return s.WatchID([]string{"goal"}), nil
		},
		report: func(string, error) {},
	})
	watcher.poll(context.Background())
	assert.Equal(t, map[string]int{"main": 1, "linked": 1}, calls)
	snapshot.Stage = "plan"
	snapshot.Artifacts = append(snapshot.Artifacts, goalreview.Artifact{Kind: "plan", Path: "plan.md", Content: "**Goal:** Feature\n### Task 1: Work\n- [ ] Test\n"})
	watcher.poll(context.Background())
	assert.Equal(t, map[string]int{"main": 2, "linked": 2}, calls)
	snapshot.Artifacts[1].Content = "**Goal:** Feature\n### Task 1: Work\n- [x] Test\n"
	watcher.poll(context.Background())
	assert.Equal(t, map[string]int{"main": 2, "linked": 2}, calls)
	roots = roots[:1]
	watcher.poll(context.Background())
	assert.NotContains(t, watcher.fingerprints, "linked")
}

func TestGoalWatchSelectedComponents(t *testing.T) {
	for _, watch := range [][]string{{"goal"}, {"kata_graph"}, {}} {
		t.Run(strings.Join(watch, "+"), func(t *testing.T) {
			snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
			calls := 0
			watcher := newGoalWatcher(goalWatchDeps{
				checkouts: func(context.Context) ([]goalCheckout, error) {
					return []goalCheckout{{root: "checkout", configRoot: "registered"}}, nil
				},
				inspect: func(context.Context, goalCheckout) (goalWatchInput, error) {
					return goalWatchInput{enabled: true, watch: watch, snapshot: snapshot}, nil
				},
				completed: func(string, []string) (string, error) { return "", nil },
				pending:   func(string, []string) ([]string, error) { return nil, nil },
				enqueue: func(_ context.Context, _ goalCheckout, s goalreview.Snapshot) (string, error) {
					calls++
					return s.WatchID(watch), nil
				},
				report: func(string, error) {},
			})
			watcher.poll(context.Background())
			initial := 0
			if len(watch) != 0 {
				initial = 1
			}
			assert.Equal(t, initial, calls)
			snapshot.Artifacts[0].Content += "Additional requirement\n"
			watcher.poll(context.Background())
			afterArtifact := initial
			if slices.Contains(watch, "goal") {
				afterArtifact++
			}
			assert.Equal(t, afterArtifact, calls)
			snapshot.Project = "project"
			snapshot.Issues = []kata.Issue{{ShortID: "aaaa", Title: "Task", Body: "Requirement", Status: "open"}}
			watcher.poll(context.Background())
			afterGraph := afterArtifact
			if slices.Contains(watch, "kata_graph") {
				afterGraph++
			}
			assert.Equal(t, afterGraph, calls)
		})
	}
}

func TestGoalWatchSkipsBareRepositoryAndKeepsLinkedCheckout(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.RunGit("commit", "--allow-empty", "-m", "Initial test commit")
	bare := filepath.Join(t.TempDir(), "bare.git")
	repo.RunGit("clone", "--bare", repo.Path(), bare)
	require.NoError(t, os.WriteFile(filepath.Join(bare, ".roborev.toml"), []byte("[goal_review]\nenabled = true\nwatch = [\"goal\"]\n"), 0o600))
	linked := filepath.Join(t.TempDir(), "linked")
	repo.RunGit("-C", bare, "worktree", "add", "--detach", linked)
	_, err := db.GetOrCreateRepo(bare)
	require.NoError(t, err)
	roots, err := server.goalCheckouts(context.Background())
	require.NoError(t, err)
	assert.Equal(
		t,
		canonicalGoalCheckoutPaths(t, []string{linked}),
		canonicalGoalCheckoutPaths(t, goalCheckoutPaths(roots)),
	)
}
