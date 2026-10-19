package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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

func TestSetupFinishedTelemetryCompletionPaths(t *testing.T) {
	for _, path := range []string{"review", "panel", "goal"} {
		t.Run(path, func(t *testing.T) {
			server, db, tmpDir := newTestServer(t)
			gitRepo := testutil.InitTestGitRepo(t, tmpDir)
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			client := &fakeTelemetryClient{enabled: true}
			server.SetTelemetry(client)
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
			assert.Equal(t, []string{"setup_finished"}, client.events)
			if reconciler != nil {
				assert.NotEmpty(t, reconciler.wakes, "committed pulls wake search")
			}
		})
	}
}

//nolint:paralleltest // changes tables in the isolated PostgreSQL fixture
func TestSyncFailedTelemetry(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("requires TEST_POSTGRES_URL pointing to an isolated PostgreSQL database")
	}
	ctx := t.Context()
	pool, err := storage.NewPgPool(ctx, url, storage.DefaultPgPoolConfig())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.EnsureSchema(ctx))
	remote := func(query string, args ...any) {
		t.Helper()
		_, err := pool.Pool().Exec(ctx, query, args...)
		require.NoError(t, err)
	}
	fail := func() {
		remote("ALTER TABLE roborev.experiment_definitions RENAME COLUMN definition_json TO unavailable")
	}
	recover := func() {
		remote("ALTER TABLE roborev.experiment_definitions RENAME COLUMN unavailable TO definition_json")
	}
	// A schema error can pass connection setup but fail the first full pass.
	fail()
	t.Cleanup(func() {
		_, _ = pool.Pool().Exec(context.Background(), "ALTER TABLE roborev.experiment_definitions RENAME COLUMN unavailable TO definition_json")
	})
	server, db, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	worker := storage.NewSyncWorker(db, config.SyncConfig{Enabled: true, PostgresURL: url, Interval: "1h"})
	require.NoError(t, worker.SetSkipInitialSync(true))
	server.SetSyncWorker(worker)
	require.NoError(t, worker.Start())
	t.Cleanup(worker.Stop)
	_, err = worker.SyncNow()
	require.ErrorContains(t, err, "pull experiment definitions")
	assert.Empty(t, client.events, "a source must complete a full pass before an outage counts")
	recover()
	_, err = worker.SyncNow()
	require.NoError(t, err)
	// Local failure leaves the established working state intact.
	_, err = db.Exec(`CREATE TRIGGER fail_local_sync BEFORE INSERT ON sync_state BEGIN SELECT RAISE(FAIL, 'local sync failure'); END`)
	require.NoError(t, err)
	_, err = worker.SyncNow()
	require.ErrorContains(t, err, "local sync failure")
	assert.Empty(t, client.events)
	_, err = db.Exec("DROP TRIGGER fail_local_sync")
	require.NoError(t, err)
	fail()
	for range 2 {
		_, err = worker.SyncNow()
		require.Error(t, err)
	}
	assert.Equal(t, []string{"sync_failed"}, client.events)
	recover()
	// Pull a remote job so the completed pass calls the progress writer.
	var repoID int64
	require.NoError(t, pool.Pool().QueryRow(ctx, "INSERT INTO roborev.repos (identity) VALUES ($1) RETURNING id", "github.com/example/sync-outcome").Scan(&repoID))
	remote(`INSERT INTO roborev.review_jobs (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id) VALUES ($1, $2, 'abc123', 'test', 'done', NOW(), $3)`, uuid.New(), repoID, uuid.New())
	callbacks := 0
	stats, err := worker.SyncNowWithProgress(func(p storage.SyncProgress) bool {
		assert.Equal(t, "pull", p.Phase)
		callbacks++
		return false // The UI disconnected after push and pull completed.
	})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PulledJobs)
	assert.Equal(t, 1, callbacks)
	fail()
	_, err = worker.SyncNow()
	require.Error(t, err)
	worker.Stop()
	assert.Equal(t, []string{"sync_failed", "sync_failed"}, client.events)
	assert.Equal(t, []map[string]any{
		{"sync_source": "postgres", "reason": "other"},
		{"sync_source": "postgres", "reason": "other"},
	}, client.properties)
}
