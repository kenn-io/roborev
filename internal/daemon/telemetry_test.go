package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type fakeTelemetryClient struct {
	enabled    bool
	events     []string
	properties []map[string]any
	captureErr error
}

func (f *fakeTelemetryClient) Capture(event string, properties map[string]any) error {
	f.events = append(f.events, event)
	f.properties = append(f.properties, properties)
	return f.captureErr
}

func (f *fakeTelemetryClient) Close() error { return nil }

func (f *fakeTelemetryClient) Enabled() bool { return f.enabled }

func TestCaptureDaemonStartedTelemetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, db, tmpDir := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)

	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:  repo.ID,
		GitRef:  "abc123",
		Agent:   "codex",
		JobType: storage.JobTypeReview,
	})
	require.NoError(err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, job.ID)
	require.NoError(err)
	require.NoError(testutil.CompleteReviewFixture(db, job.ID, "codex", "prompt", "No issues found."))
	for i := range 36 {
		_, err := db.GetOrCreateRepo(filepath.Join(tmpDir, fmt.Sprintf("repo-%d", i)))
		require.NoError(err)
	}

	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 4
	cfg.Sync.Enabled = true
	cfg.CI.Enabled = true
	cfg.AutoDesignReview.Enabled = true

	server.captureDaemonStartedTelemetry(cfg)

	require.Equal([]string{"daemon_started", "setup_finished"}, client.events)
	require.Len(client.properties, 2)
	assert.Equal("21-100", client.properties[0]["repo_count"])
	assert.Equal("0-5", client.properties[0]["review_count"])
	assert.Equal(true, client.properties[0]["sync_enabled"])
	assert.Equal(true, client.properties[0]["ci_enabled"])
	assert.Equal(true, client.properties[0]["auto_design_enabled"])
}

func TestStartDailyTelemetryLoopCapturesImmediately(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server.startDailyTelemetryLoop(ctx, config.DefaultConfig())

	require.Len(client.events, 1)
	require.Len(client.properties, 1)
	assert.Equal("daemon_active", client.events[0])
}

func TestCaptureDaemonStartedTelemetryDisabledNoops(t *testing.T) {
	assert := assert.New(t)

	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: false}
	server.SetTelemetry(client)

	server.captureDaemonStartedTelemetry(config.DefaultConfig())

	assert.Empty(client.events)
	assert.Nil(client.properties)
}

func TestSetupFinishedTelemetryClaimAndDelivery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		client     *fakeTelemetryClient
		wantClaim  string
		wantEvents []string
	}{
		{name: "successful delivery", client: &fakeTelemetryClient{enabled: true}, wantClaim: "1", wantEvents: []string{"setup_finished"}},
		{name: "unset telemetry"},
		{name: "disabled telemetry", client: &fakeTelemetryClient{}},
		{name: "failed delivery", client: &fakeTelemetryClient{enabled: true, captureErr: errors.New("collector unavailable")}, wantClaim: "1", wantEvents: []string{"setup_finished"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, db, tmpDir := newTestServer(t)
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
			require.NoError(t, err)
			_, err = db.ClaimJob("worker")
			require.NoError(t, err)
			require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", "No issues found."))
			if tc.client != nil {
				server.SetTelemetry(tc.client)
			}
			server.captureSetupFinishedTelemetry()
			state, err := db.GetSyncState(storage.SyncStateSetupFinished)
			require.NoError(t, err)
			assert.Equal(t, tc.wantClaim, state)
			if tc.client != nil {
				server.captureSetupFinishedTelemetry()
				assert.Equal(t, tc.wantEvents, tc.client.events)
				if !tc.client.enabled {
					tc.client.enabled = true
					server.captureSetupFinishedTelemetry()
					server.captureSetupFinishedTelemetry()
					assert.Equal(t, []string{"setup_finished"}, tc.client.events)
				}
			}
		})
	}
}

func TestSetupFinishedTelemetrySkipsLaterClaims(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	_, err = db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", "No issues found."))
	_, err = db.Exec(`CREATE TABLE setup_claim_attempts (value TEXT);
		CREATE TRIGGER count_setup_claim BEFORE INSERT ON sync_state
		WHEN NEW.key = 'telemetry.setup_finished'
		BEGIN INSERT INTO setup_claim_attempts VALUES (NEW.value); END`)
	require.NoError(t, err)
	var callers sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		callers.Go(func() {
			<-start
			server.captureSetupFinishedTelemetry()
		})
	}
	close(start)
	callers.Wait()
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM setup_claim_attempts`).Scan(&attempts))
	assert.Equal(t, 1, attempts)
	assert.Equal(t, []string{"setup_finished"}, client.events)
	for _, restart := range []bool{false, true} {
		if restart {
			require.NoError(t, db.SetSyncState(storage.SyncStateSetupFinished, "1"))
			server = newServerWithLogs(db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { _ = server.Close() })
			server.SetTelemetry(client)
			server.captureSetupFinishedTelemetry()
		}
		_, err = db.Exec(`DELETE FROM sync_state WHERE key = ?`, storage.SyncStateSetupFinished)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM setup_claim_attempts`)
		require.NoError(t, err)
		server.captureSetupFinishedTelemetry()
		server.workerPool.captureSetupFinished()
		var attempts int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM setup_claim_attempts`).Scan(&attempts))
		assert.Zero(t, attempts)
		assert.Equal(t, []string{"setup_finished"}, client.events)
	}
}

func TestSetupFinishedTelemetryRetriesStartupRead(t *testing.T) {
	_, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	_, err = db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", "No issues found."))
	require.NoError(t, db.SetSyncState(storage.SyncStateSetupFinished, "1"))
	_, err = db.Exec(`ALTER TABLE sync_state RENAME TO saved_sync_state`)
	require.NoError(t, err)
	server := newServerWithLogs(db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	server.captureSetupFinishedTelemetry()
	_, err = db.Exec(`ALTER TABLE saved_sync_state RENAME TO sync_state`)
	require.NoError(t, err)
	server.captureSetupFinishedTelemetry()
	_, err = db.Exec(`DELETE FROM sync_state WHERE key = ?`, storage.SyncStateSetupFinished)
	require.NoError(t, err)
	server.captureSetupFinishedTelemetry()
	server.workerPool.captureSetupFinished()
	marker, err := db.GetSyncState(storage.SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Empty(t, marker)
	assert.Empty(t, client.events)
}

func TestSetupFinishedTelemetryGateDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &fakeTelemetryClient{enabled: true}
		server := &Server{telemetry: client, setupFinishedGate: make(chan struct{}, 1)}
		server.setupFinishedGate <- struct{}{}
		start := time.Now()
		server.captureSetupFinishedTelemetry()
		assert.Equal(t, 50*time.Millisecond, time.Since(start))
		assert.Empty(t, client.events)
	})
}

func TestSetupFinishedTelemetryBusyWriterRetries(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	server.captureSetupFinishedTelemetry()
	assert.Empty(t, client.events, "no eligible review remains retryable")
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
	require.NoError(t, err)
	_, err = db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "", "No issues found."))
	writer, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	_, err = writer.Exec(`INSERT INTO sync_state (key, value) VALUES ('writer-lock', 'held')`)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.captureSetupFinishedTelemetry()
	}()
	// Wall-clock wait: SQLite busy handling runs outside synctest.
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	assert.Empty(t, client.events)
	marker, err := db.GetSyncState(storage.SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Empty(t, marker)
	var busyTimeout int
	require.NoError(t, db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, 30000, busyTimeout)
	require.NoError(t, writer.Rollback())
	server.captureSetupFinishedTelemetry()
	assert.Equal(t, []string{"setup_finished"}, client.events)
}

func TestSetupFinishedTelemetryCompletionPaths(t *testing.T) {
	for _, path := range []string{"review", "panel", "goal"} {
		t.Run(path, func(t *testing.T) {
			server, db, tmpDir := newTestServer(t)
			gitRepo := testutil.InitTestGitRepo(t, tmpDir)
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			client := &fakeTelemetryClient{enabled: true}
			server.SetTelemetry(client)
			server.workerPool.captureSetupFinished = func() {
				require.True(t, server.workerPool.attemptTransitionsMu.TryLock(), "setup capture must follow the attempt transition")
				server.workerPool.attemptTransitionsMu.Unlock()
				server.captureSetupFinishedTelemetry()
			}
			var job *storage.ReviewJob
			switch path {
			case "review":
				job, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: testutil.GetHeadSHA(t, tmpDir), Agent: "test"})
				require.NoError(t, err)
				claimed, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, claimed)
				server.workerPool.processJob(testWorkerID, claimed)
			case "panel":
				tc := &workerTestContext{DB: db, TmpDir: tmpDir, GitRepo: gitRepo, Repo: repo, Pool: server.workerPool, Broadcaster: server.broadcaster}
				run, members, _ := enqueuePanelRun(t, tc, "setup-panel", []memberSpec{{name: "first", agent: "test"}})
				claimed, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, claimed)
				require.Equal(t, members[0].ID, claimed.ID)
				server.workerPool.processJob(testWorkerID, claimed)
				member, err := db.GetJobByID(claimed.ID)
				require.NoError(t, err)
				require.Equal(t, storage.JobStatusDone, member.Status)
				_, err = db.GetReviewByJobID(member.ID)
				require.NoError(t, err)
				require.Equal(t, []string{"setup_finished"}, client.events)
				job = releaseAndClaimSynthesis(t, tc, run)
				server.workerPool.processSynthesisJob(t.Context(), testWorkerID, job)
			case "goal":
				registerGoalReviewPi(t, server.configWatcher.Config())
				snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
				job, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: snapshot.ID(), Agent: "pi", ReviewType: "goal", JobType: storage.JobTypeGoalReview, Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true})
				require.NoError(t, err)
				server.workerPool.goalReviewRunner = goalReviewTestRunner(func(context.Context, goalreview.Snapshot, prompt.SnapshotResult) ([]goalreview.Finding, error) {
					return nil, nil
				})
				claimed, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, claimed)
				server.workerPool.processJob(testWorkerID, claimed)
			}
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, stored.Status)
			_, err = db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, []string{"setup_finished"}, client.events)
		})
	}
}

func TestSetupFinishedTelemetryAfterSyncPullWrite(t *testing.T) {
	for _, path := range []string{"search", "without search"} {
		t.Run(path, func(t *testing.T) {
			server, db, tmpDir := newTestServer(t)
			client := &fakeTelemetryClient{enabled: true}
			server.SetTelemetry(client)
			var reconciler *recordingSearchReconciler
			if path == "search" {
				reconciler = newRecordingSearchReconciler()
				server.searchReconciler = reconciler
			}
			server.SetSyncWorker(storage.NewSyncWorker(db, config.SyncConfig{}))
			require.Empty(t, client.events)
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			now := time.Now().UTC().Truncate(time.Second)
			job := storage.PulledJob{UUID: uuid.New(), GitRef: "abc123", Agent: "test", JobType: storage.JobTypeReview, ReviewType: "code", Status: "applied", EnqueuedAt: now, FinishedAt: &now, SourceMachineID: uuid.New(), UpdatedAt: now}
			require.NoError(t, db.UpsertPulledJob(job, repo.ID, nil))
			require.NoError(t, db.UpsertPulledReview(storage.PulledReview{UUID: uuid.New(), JobUUID: job.UUID, JobEnqueuedAt: now, JobSourceMachineID: job.SourceMachineID, Agent: "test", StructuredOutput: testutil.ReviewFixtureJSON("No issues found."), UpdatedByMachineID: job.SourceMachineID, CreatedAt: now, UpdatedAt: now}))
			server.afterSyncPullWrite()
			require.Empty(t, client.events, "per-row search wake leaves setup checking to the pull boundary")
			server.captureSetupFinishedTelemetry()
			assert.Equal(t, []string{"setup_finished"}, client.events)
			if reconciler != nil {
				assert.NotEmpty(t, reconciler.wakes, "committed pulls wake search")
			}
		})
	}
}
