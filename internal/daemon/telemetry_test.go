package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"go.kenn.io/roborev/internal/testutil/setupclaim"
)

type fakeTelemetryClient struct {
	mu         sync.Mutex
	enabled    bool
	events     []string
	properties []map[string]any
	captureErr error
}

func (f *fakeTelemetryClient) Capture(event string, properties map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	f.properties = append(f.properties, properties)
	return f.captureErr
}

func (f *fakeTelemetryClient) recordedEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeTelemetryClient) recordedProperties() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.properties...)
}

func waitSetupFinished(t *testing.T, client *fakeTelemetryClient) {
	t.Helper()
	// Wall-clock wait: setup claims use real SQLite connections.
	require.Eventually(t, func() bool {
		return slices.Contains(client.recordedEvents(), "setup_finished")
	}, 5*time.Second, time.Millisecond)
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
	waitSetupFinished(t, client)

	require.Equal([]string{"daemon_started", "setup_finished"}, client.recordedEvents())
	require.Len(client.recordedProperties(), 2)
	assert.Equal("21-100", client.recordedProperties()[0]["repo_count"])
	assert.Equal("0-5", client.recordedProperties()[0]["review_count"])
	assert.Equal(true, client.recordedProperties()[0]["sync_enabled"])
	assert.Equal(true, client.recordedProperties()[0]["ci_enabled"])
	assert.Equal(true, client.recordedProperties()[0]["auto_design_enabled"])
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

	require.Len(client.recordedEvents(), 1)
	require.Len(client.recordedProperties(), 1)
	assert.Equal("daemon_active", client.recordedEvents()[0])
}

func TestCaptureDaemonStartedTelemetryDisabledNoops(t *testing.T) {
	assert := assert.New(t)

	server, _, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: false}
	server.SetTelemetry(client)

	server.captureDaemonStartedTelemetry(config.DefaultConfig())

	assert.Empty(client.recordedEvents())
	assert.Nil(client.recordedProperties())
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
			if tc.wantClaim != "" {
				waitSetupFinished(t, tc.client)
			}
			state, err := db.GetSyncState(storage.SyncStateSetupFinished)
			require.NoError(t, err)
			assert.Equal(t, tc.wantClaim, state)
			if tc.client != nil {
				server.captureSetupFinishedTelemetry()
				assert.Equal(t, tc.wantEvents, tc.client.recordedEvents())
				if !tc.client.enabled {
					tc.client.enabled = true
					server.captureSetupFinishedTelemetry()
					server.captureSetupFinishedTelemetry()
					waitSetupFinished(t, tc.client)
					assert.Equal(t, []string{"setup_finished"}, tc.client.recordedEvents())
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
	claimAttempts := setupclaim.InstallCounter(t, db.DB)
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
	waitSetupFinished(t, client)
	assert.Equal(t, 1, claimAttempts())
	assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
	_, err = db.Exec(`DELETE FROM sync_state WHERE key = ?`, storage.SyncStateSetupFinished)
	require.NoError(t, err)
	server.captureSetupFinishedTelemetry()
	server.workerPool.captureSetupFinished()
	assert.Equal(t, 1, claimAttempts())
	assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
}

func TestSetupFinishedTelemetryLoadsSavedMarker(t *testing.T) {
	server, db, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	require.NoError(t, db.SetSyncState(storage.SyncStateSetupFinished, "1"))
	server.captureSetupFinishedTelemetry()
	// Wall-clock wait: startup reads the persisted SQLite marker.
	require.Eventually(t, server.setupFinishedClaimed.Load, time.Second, time.Millisecond)
	_, err := db.Exec(`DELETE FROM sync_state WHERE key = ?`, storage.SyncStateSetupFinished)
	require.NoError(t, err)
	server.captureSetupFinishedTelemetry()
	server.workerPool.captureSetupFinished()
	require.NoError(t, server.Close())
	marker, err := db.GetSyncState(storage.SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Empty(t, marker)
	assert.Empty(t, client.recordedEvents())
}

func TestSetupFinishedTelemetryBusyWriterRetries(t *testing.T) {
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
	var path string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	connector := &setupQueryConnector{driver: db.Driver(), dsn: path + "?_pragma=busy_timeout(30000)", busy: make(chan struct{}, 1)}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	server.db = &storage.DB{DB: pool}
	writer, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	_, err = writer.Exec(`INSERT INTO sync_state (key, value) VALUES ('writer-lock', 'held')`)
	require.NoError(t, err)
	server.captureSetupFinishedTelemetry()
	// Wall-clock wait: the SQLite marker write must report actual contention.
	require.Eventually(t, func() bool { return len(connector.busy) == 1 }, time.Second, time.Millisecond)
	assert.Empty(t, client.recordedEvents())
	marker, err := db.GetSyncState(storage.SyncStateSetupFinished)
	require.NoError(t, err)
	assert.Empty(t, marker)
	require.NoError(t, writer.Rollback())
	waitSetupFinished(t, client)
	var busyTimeout int
	require.NoError(t, pool.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, 30000, busyTimeout)
	assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
}

type setupQueryConnector struct {
	driver  driver.Driver
	dsn     string
	started chan struct{}
	release <-chan struct{}
	checks  atomic.Int32
	busy    chan struct{}
}

func (c *setupQueryConnector) Driver() driver.Driver { return c.driver }
func (c *setupQueryConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &setupQueryConn{Conn: conn, connector: c}, nil
}

type setupQueryConn struct {
	driver.Conn
	connector *setupQueryConnector
}

func (c *setupQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.connector.started != nil && strings.Contains(query, "SELECT EXISTS") {
		c.connector.checks.Add(1)
		c.connector.started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.connector.release:
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *setupQueryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if c.connector.busy != nil && storage.IsSQLiteBusy(err) {
		select {
		case c.connector.busy <- struct{}{}:
		default:
		}
	}
	return result, err
}

func TestSetupFinishedTelemetryCoalescesAndStops(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			server, db, _ := newTestServer(t)
			client := &fakeTelemetryClient{enabled: true}
			server.SetTelemetry(client)
			var path string
			require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
			release := make(chan struct{})
			connector := &setupQueryConnector{driver: db.Driver(), dsn: path, started: make(chan struct{}, 2), release: release}
			pool := sql.OpenDB(connector)
			t.Cleanup(func() { require.NoError(t, pool.Close()) })
			// The server owns the wrapper; the fixture still owns the original pool.
			server.db = &storage.DB{DB: pool}
			server.captureSetupFinishedTelemetry()
			// Wall-clock wait: the worker reaches the SQLite driver boundary.
			require.Eventually(t, func() bool { return len(connector.started) == 1 }, time.Second, time.Millisecond)
			<-connector.started
			for range 100 {
				server.captureSetupFinishedTelemetry()
			}
			if !shutdown {
				close(release)
				// Wall-clock wait: the queued check uses the real SQLite connection.
				require.Eventually(t, func() bool { return len(connector.started) == 1 }, time.Second, time.Millisecond)
				<-connector.started
			}
			require.NoError(t, server.Close())
			<-server.setupWorkerDone
			if shutdown {
				assert.Equal(t, int32(1), connector.checks.Load())
			} else {
				assert.Equal(t, int32(2), connector.checks.Load())
			}
			assert.Empty(t, client.recordedEvents())
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
				waitSetupFinished(t, client)
				require.Equal(t, []string{"setup_finished"}, client.recordedEvents())
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
			waitSetupFinished(t, client)
			assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
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
			require.Empty(t, client.recordedEvents())
			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			now := time.Now().UTC().Truncate(time.Second)
			job := storage.PulledJob{UUID: uuid.New(), GitRef: "abc123", Agent: "test", JobType: storage.JobTypeReview, ReviewType: "code", Status: "applied", EnqueuedAt: now, FinishedAt: &now, SourceMachineID: uuid.New(), UpdatedAt: now}
			require.NoError(t, db.UpsertPulledJob(job, repo.ID, nil))
			require.NoError(t, db.UpsertPulledReview(storage.PulledReview{UUID: uuid.New(), JobUUID: job.UUID, JobEnqueuedAt: now, JobSourceMachineID: job.SourceMachineID, Agent: "test", StructuredOutput: testutil.ReviewFixtureJSON("No issues found."), UpdatedByMachineID: job.SourceMachineID, CreatedAt: now, UpdatedAt: now}))
			server.afterSyncPullWrite()
			require.Empty(t, client.recordedEvents(), "per-row search wake leaves setup checking to the pull boundary")
			server.captureSetupFinishedTelemetry()
			waitSetupFinished(t, client)
			assert.Equal(t, []string{"setup_finished"}, client.recordedEvents())
			if reconciler != nil {
				assert.NotEmpty(t, reconciler.wakes, "committed pulls wake search")
			}
		})
	}
}
