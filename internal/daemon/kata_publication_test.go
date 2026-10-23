package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestKataDaemonStartupDoesNotBackfill(t *testing.T) {
	testenv.SetDataDir(t)
	db, _ := testutil.OpenTestDBWithDir(t)
	repo := testutil.CreateTestRepo(t, db)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "historical-review", "test", findingsReviewDocument)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.Integrations.Kata.Enabled = true
	cfg.Integrations.Kata.Endpoint = "https://kata.example"
	cfg.Integrations.Kata.APIKey = kataTestCredential
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Project: "reviews", Findings: "comments"}}
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { _ = server.Close() })
	errCh, _ := startServerAndWaitForRuntime(t, server)
	stopTestServer(t, server, errCh)
	records, err := db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Empty(t, records, "actual startup must not authorize historical publication")
}

func TestKataDestinationLookupDoesNotHoldAttemptGuard(t *testing.T) {
	server, db, _ := newTestServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(api.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	cfg := server.configWatcher.Config()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: api.URL, APIKey: kataTestCredential}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.failed", Project: "reviews"}}
	repo := testutil.CreateTestRepo(t, db)
	_, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "first", Agent: "test"})
	require.NoError(t, err)
	first, err := db.ClaimJob("worker-one")
	require.NoError(t, err)
	require.NotNil(t, first)
	_, err = db.Exec(`UPDATE review_jobs SET retry_count=? WHERE id=?`, maxRetries, first.ID)
	require.NoError(t, err)
	_, err = db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "second", Agent: "test"})
	require.NoError(t, err)
	second, err := db.ClaimJob("worker-two")
	require.NoError(t, err)
	require.NotNil(t, second)
	terminal := testutil.CreateCompletedReview(t, db, repo.ID, "terminal", "test", findingsReviewDocument)
	failed := make(chan struct{})
	go func() {
		defer close(failed)
		server.workerPool.failOrRetry("worker-one", first, "test", "agent failure")
	}()
	<-started
	var registered atomic.Bool
	registrationDone := make(chan error, 1)
	go func() {
		releaseAttempt, err := db.StartJobAttempt(t.Context(), second, "worker-two", func() {})
		if err == nil {
			releaseAttempt()
			err = db.ReenqueueJob(terminal.ID, storage.ReenqueueOpts{})
			if err == nil {
				_, err = db.RequeueUpdateInterruptedJob(first.ID, "worker-one")
			}
			registered.Store(err == nil)
		}
		registrationDone <- err
	}()
	// Wall-clock wait: the other transition uses SQLite and the lookup is
	// deliberately held in an HTTP handler outside synctest's control.
	assert.Eventually(t, registered.Load, time.Second, time.Millisecond)
	releaseOnce.Do(func() { close(release) })
	<-failed
	require.NoError(t, <-registrationDone)
	current, err := db.GetJobByID(first.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, current.Status)
	records, err := db.KataHandoffs(t.Context(), first.ID)
	require.NoError(t, err)
	assert.Empty(t, records, "a displaced attempt must not persist its prepared handoff")
}

func TestKataHookDeliveryModeSurvivesConfigurationReload(t *testing.T) {
	for _, enabledAtCapture := range []bool{false, true} {
		t.Run(fmt.Sprint(enabledAtCapture), func(t *testing.T) {
			marker := kataOnPathMarker(t)
			server, _ := kataAPIServer(t)
			cfg := server.configWatcher.Config()
			cfg.Integrations.Kata.Enabled = enabledAtCapture
			cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.failed", Project: "reviews"}}
			// Hold hook dispatch until the completion transaction and event have
			// finished, then reload configuration before delivering that event.
			broadcaster := NewBroadcaster()
			server.workerPool.broadcaster = broadcaster
			_, events := broadcaster.Subscribe("")
			repo := testutil.CreateTestRepo(t, server.db)
			_, err := server.db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "reload", Agent: "test"})
			require.NoError(t, err)
			job, err := server.db.ClaimJob("worker")
			require.NoError(t, err)
			require.NotNil(t, job)
			_, err = server.db.Exec(`UPDATE review_jobs SET retry_count=? WHERE id=?`, maxRetries, job.ID)
			require.NoError(t, err)
			server.workerPool.failOrRetry("worker", job, "test", "synthetic failure")
			event := <-events
			require.Equal(t, "review.failed", event.Type)
			records, err := server.db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			cfg.Integrations.Kata.Enabled = !enabledAtCapture
			server.hookRunner.handleEvent(event)
			server.hookRunner.wg.Wait()
			if enabledAtCapture {
				assert.Len(t, records, 1)
				assert.NoFileExists(t, marker, "a captured API operation must not also run a CLI hook")
			} else {
				assert.Empty(t, records)
				data, err := os.ReadFile(marker)
				require.NoError(t, err, "enabling the API later must not suppress the captured CLI delivery")
				assert.Contains(t, string(data), "create")
			}
		})
	}
}

func TestKataEligibilityOnlyAffectsKata(t *testing.T) {
	for _, mode := range []string{"", "comments", "summary"} {
		for _, eventType := range []string{"review.completed", "review.failed"} {
			for _, job := range []storage.ReviewJob{
				{ID: 1, JobType: storage.JobTypeReview, Source: storage.JobSourceCI},
				{ID: 2, JobType: storage.JobTypeReview, PanelRole: storage.PanelRoleMember},
			} {
				t.Run(mode+eventType+string(rune('0'+job.ID)), func(t *testing.T) {
					event := eventForJob(eventType, &job, job.ID)
					event.Verdict = "F"
					_, ok := kataCreateRequest(config.HookConfig{Type: "kata", Findings: mode}, event)
					assert.False(t, ok)
					assert.False(t, event.SuppressHooks, "other hook types still receive CI/member events")
				})
			}
		}
	}
}

func TestKataSummaryFreezesPolicyAndDestination(t *testing.T) {
	id := uuid.New()
	job := &storage.ReviewJob{ID: 7, UUID: &id, JobType: storage.JobTypeReview, RepoPath: t.TempDir(), RepoName: "sample", GitRef: "abc123"}
	c := storage.KataCompletion{Job: job, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail, Output: "SECRET_SENTINEL", At: time.Now(), Snapshots: []storage.FindingSnapshot{{Selector: "f:example", ReviewUUID: uuid.New(), DocumentHash: strings.Repeat("a", 64), Index: 0, Finding: structuredreview.Finding{Severity: "high", Problem: "SECRET_SENTINEL", Fix: "SECRET_SENTINEL", Location: "SECRET_SENTINEL"}}}}
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews", Teammate: "reviewer"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
	resolver := func(_ context.Context, endpoint, project string) (kata.Binding, error) {
		return kata.Binding{Project: project, ProjectUID: "pinned-project"}, nil
	}
	requests, err := buildKataPublications(t.Context(), cfg, c, resolver)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	r := requests[0]
	assert.Equal(t, "https://kata.example", r.Endpoint)
	assert.Equal(t, "pinned-project", r.ProjectUID)
	assert.Equal(t, "reviewer", r.Teammate)
	assert.NotContains(t, r.Create.Body, "SECRET_SENTINEL")
	require.Len(t, r.Entries, 1)
	assert.NotContains(t, r.Entries[0].Body, "SECRET_SENTINEL")
	cfg.Hooks[0].Findings = "comments"
	cfg.Integrations.Kata.DefaultProject = "elsewhere"
	assert.Equal(t, "summary", r.Policy)
	assert.Equal(t, "reviews", r.Project)
	c.Error = "SECRET_SENTINEL"
	c.ReviewUUID = ""
	c.Snapshots = nil
	cfg.Hooks[0].Findings = "summary"
	requests, err = buildKataPublications(t.Context(), cfg, c, resolver)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0].Create.Body, "SECRET_SENTINEL")
}

func TestKataDistinctHooksDoNotConflict(t *testing.T) {
	id := uuid.New()
	c := storage.KataCompletion{Job: &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: t.TempDir()}, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail}
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
	priority := 1
	cfg.Hooks = []config.HookConfig{
		{Type: "kata", Event: "review.*", Findings: "comments"},
		{Type: "kata", Event: "review.*", Findings: "comments", Labels: []string{"extra"}},
		{Type: "kata", Event: "review.*", Findings: "comments", Priority: &priority},
		{Type: "kata", Event: "review.*", Findings: "comments", Project: "other-project"},
		{Type: "kata", Event: "review.*", Findings: "summary"},
	}
	requests, err := buildKataPublications(t.Context(), cfg, c, func(_ context.Context, _ string, project string) (kata.Binding, error) {
		return kata.Binding{Project: project, ProjectUID: "uid-" + project}, nil
	})
	require.NoError(t, err)
	require.Len(t, requests, 5)
	identities := map[string]bool{}
	for _, r := range requests {
		assert.NotContains(t, identities, r.OperationID)
		identities[r.OperationID] = true
	}
}

// restartKataAPI supplies the external receipts while the real SQLite queue,
// publisher loop, and policy builder are exercised together.
type restartKataAPI struct {
	katapublish.API
	db                    *storage.DB
	operation             string
	endpoint              string
	project               string
	uid                   string
	issue                 kata.Issue
	offline               bool
	failAfterRegistration bool
	creates, comments     int
}

func (f *restartKataAPI) Endpoint() string { return f.endpoint }
func (f *restartKataAPI) ResolveProject(_ context.Context, project string) (kata.Binding, error) {
	if f.offline {
		return kata.Binding{}, errors.New("offline")
	}
	if project != f.project {
		return kata.Binding{}, errors.New("project changed")
	}
	return kata.Binding{Project: project, ProjectUID: f.uid}, nil
}

func (f *restartKataAPI) ListProject(context.Context, string, kata.ListOpts) ([]kata.Issue, error) {
	if f.issue.UID == "" {
		return nil, nil
	}
	return []kata.Issue{f.issue}, nil
}

func (f *restartKataAPI) ShowComplete(context.Context, string) (kata.Issue, error) {
	return f.issue, nil
}

func (f *restartKataAPI) Create(_ context.Context, r kata.CreateReq) (kata.CreateResult, error) {
	var stateJSON string
	if err := f.db.QueryRow(`SELECT state_json FROM kata_handoffs WHERE operation_id=?`, f.operation).Scan(&stateJSON); err != nil {
		return kata.CreateResult{}, err
	}
	var state katapublish.State
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		return kata.CreateResult{}, err
	}
	if state.ProjectUID != f.uid {
		return kata.CreateResult{}, errors.New("remote write before durable UID pin")
	}
	f.creates++
	f.issue = kata.Issue{Status: "open", UID: "issue-1", ShortID: "one", QualifiedID: f.project + "#one", ProjectUID: f.uid, Metadata: r.Metadata}
	return kata.CreateResult{IssueUID: "issue-1", ShortID: "one", ProjectUID: f.uid}, nil
}

func (f *restartKataAPI) Comment(_ context.Context, r kata.CommentReq) (kata.CommentResult, error) {
	if f.failAfterRegistration {
		return kata.CommentResult{}, errors.New("disconnected after pending registration")
	}
	f.comments++
	c := kata.Comment{UID: "comment-1", IssueUID: f.issue.UID, Body: r.Body, Author: "service", Teammate: r.Teammate}
	f.issue.Comments = append(f.issue.Comments, c)
	return kata.CommentResult{UID: c.UID, Body: c.Body, Attribution: c.Attribution}, nil
}

func (f *restartKataAPI) MutateMetadata(_ context.Context, _ string, key string, _ any, value any) (kata.Issue, error) {
	f.issue.Metadata[key] = value
	return f.issue, nil
}

func TestKataOfflineRestartUsesFrozenPublication(t *testing.T) {
	db, dir := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews", APIKey: "original-key"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
	repoPath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".kata.local.toml"), []byte("[project]\nname = 'reviews'\n"), 0o600))
	job := createTestJob(t, db, repoPath, "abc123", "test")
	db.SetKataHandoffBuilder(func(ctx context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
		return buildKataPublications(ctx, cfg, c, func(context.Context, string, string) (kata.Binding, error) {
			return kata.Binding{}, errors.New("initial outage")
		})
	})
	_, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(job.ID, "test", `{"schema_version":2,"summary":"SECRET_SENTINEL","verdict":"fail","findings":[{"severity":"high","problem":"SECRET_SENTINEL","fix":"SECRET_SENTINEL","location":null}]}`))
	saved, err := db.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	assert.Empty(t, saved[0].Request.ProjectUID)
	require.NoError(t, db.Close())
	reopened, err := storage.Open(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	defer reopened.Close()
	// Neither new hook settings, binding nor default project authorize a reroute.
	cfg.Hooks[0].Findings = "comments"
	cfg.Integrations.Kata.DefaultProject = "elsewhere"
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".kata.local.toml"), []byte("[project]\nname = 'elsewhere'\n"), 0o600))
	api := &restartKataAPI{db: reopened, operation: saved[0].OperationID, endpoint: "https://kata.example", project: "reviews", uid: "project-1", failAfterRegistration: true}
	watcher := NewConfigWatcher("", cfg, nil, nil)
	publisher := &kataPublisher{db: reopened, config: watcher, newAPI: func(config.KataIntegrationConfig) (katapublish.API, error) { return api, nil }}
	claimed, err := reopened.ClaimKataHandoff(t.Context(), time.Now())
	require.NoError(t, err)
	require.NotNil(t, claimed)
	publisher.deliver(t.Context(), *claimed)
	saved, err = reopened.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "pending", saved[0].Status)
	assert.Equal(t, "project-1", saved[0].State.ProjectUID)
	assert.Equal(t, 1, api.creates)
	// Simulate restart after pending registration. The startup queue must finish
	// without a CLI or another completion event. SQLite is external to synctest.
	api.failAfterRegistration = false
	_, err = reopened.Exec(`UPDATE kata_handoffs SET next_attempt=0`)
	require.NoError(t, err)
	publisher.start(t.Context())
	defer publisher.stop()
	require.Eventually(t, func() bool {
		records, e := reopened.KataHandoffs(context.Background(), job.ID)
		return e == nil && len(records) == 1 && records[0].Status == "done"
	}, 10*time.Second, 10*time.Millisecond)
	publisher.stop()
	assert.Equal(t, 1, api.creates)
	assert.Equal(t, 1, api.comments)
	payload, err := json.Marshal(api.issue)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "SECRET_SENTINEL")
}

func TestKataActualFailedBroadcastSuppressesCIAndPanelMembers(t *testing.T) {
	for _, job := range []storage.ReviewJob{{ID: 1, JobType: storage.JobTypeReview, Source: storage.JobSourceCI}, {ID: 2, JobType: storage.JobTypeReview, CIBaseBranch: "main"}, {ID: 3, JobType: storage.JobTypeReview, PanelRole: storage.PanelRoleMember}} {
		broadcaster := NewBroadcaster()
		_, events := broadcaster.Subscribe("")
		pool := &WorkerPool{broadcaster: broadcaster}
		pool.broadcastFailed(&job, "test", "synthetic failure")
		event := <-events
		assert.True(t, event.SuppressKata)
		assert.False(t, event.SuppressHooks)
	}
}

func TestKataActualMemberCompletedBroadcast(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "test")
	job.PanelRole = storage.PanelRoleMember
	_, events := tc.Broadcaster.Subscribe("")
	tc.Pool.processJob(testWorkerID, job)
	first := <-events
	require.Equal(t, "review.started", first.Type)
	completed := <-events
	require.Equal(t, "review.completed", completed.Type)
	assert.True(t, completed.SuppressKata)
	assert.False(t, completed.SuppressHooks)
}

func FuzzSummaryPublicationDoesNotDiscloseText(f *testing.F) {
	f.Add("hidden finding")
	f.Add("\x00\xff\n秘密")
	repo := f.TempDir()
	f.Fuzz(func(t *testing.T, text string) {
		secret := "PRIVATE_SENTINEL:" + text
		id := uuid.New()
		reviewID := uuid.New()
		c := storage.KataCompletion{Job: &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: repo}, ReviewUUID: reviewID.String(), Verdict: storage.VerdictFail, Output: secret, Error: secret, Snapshots: []storage.FindingSnapshot{{Selector: "f:example", ReviewUUID: reviewID, DocumentHash: strings.Repeat("a", 64), Finding: structuredreview.Finding{Severity: "high", Problem: secret, Fix: secret, Location: secret}}}}
		cfg := config.DefaultConfig()
		cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
		cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
		requests, err := buildKataPublications(t.Context(), cfg, c, func(context.Context, string, string) (kata.Binding, error) {
			return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
		})
		require.NoError(t, err)
		require.Len(t, requests, 1)
		payload, err := json.Marshal(requests[0])
		require.NoError(t, err)
		assert.NotContains(t, string(payload), "PRIVATE_SENTINEL:")
	})
}

func TestKataReviewHookDoesNotAuthorizeGoalReview(t *testing.T) {
	id := uuid.New()
	job := &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeGoalReview, RepoPath: t.TempDir()}
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "comments"}}
	for _, failure := range []string{"", "synthetic failure"} {
		got, err := buildKataPublications(t.Context(), cfg, storage.KataCompletion{Job: job, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail, Error: failure}, func(context.Context, string, string) (kata.Binding, error) {
			return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
		})
		require.NoError(t, err)
		assert.Empty(t, got)
	}
}

func TestKataCredentialFileRotationCannotChangeCheckedCredential(t *testing.T) {
	db := testutil.OpenTestDB(t)
	keyFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(keyFile, []byte("original-key"), 0o600))
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", APIKeyFile: keyFile, DefaultProject: "reviews"}
	job := createTestJob(t, db, t.TempDir(), "credential123", "test")
	req := katapublish.Request{OperationID: "publication-credential", Endpoint: "https://kata.example", Project: "reviews", ProjectUID: "project-1", Policy: "legacy", ReviewUUID: uuid.New().String(), Teammate: "reviewer", CredentialFingerprint: kataCredentialFingerprint(cfg.Integrations.Kata), Create: kata.CreateReq{Title: "Review findings"}}
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, storage.InsertKataHandoffTx(t.Context(), tx, job.ID, req))
	require.NoError(t, tx.Commit())
	h, err := db.ClaimKataHandoff(t.Context(), time.Now())
	require.NoError(t, err)
	require.NotNil(t, h)
	called := false
	publisher := &kataPublisher{db: db, config: NewStaticConfig(cfg), newAPI: func(snapshot config.KataIntegrationConfig) (katapublish.API, error) {
		called = true
		require.NoError(t, os.WriteFile(keyFile, []byte("replacement-key"), 0o600))
		key, err := snapshot.ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "original-key", key, "the constructor must receive exactly the credential whose fingerprint passed")
		assert.Empty(t, snapshot.APIKeyFile, "construction must not reread a rotated file")
		return nil, errors.New("stop after validating constructor input")
	}}
	publisher.deliver(t.Context(), *h)
	assert.True(t, called)
}

func TestKataPublicationAuthorizesDistinctIssueAfterMarkerLookup(t *testing.T) {
	id := uuid.New()
	job := &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: t.TempDir()}
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
	got, err := buildKataPublications(t.Context(), cfg, storage.KataCompletion{Job: job, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail}, func(context.Context, string, string) (kata.Binding, error) {
		return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	data, err := json.Marshal(got[0].Create)
	require.NoError(t, err)
	var captured map[string]any
	require.NoError(t, json.Unmarshal(data, &captured))
	assert.Equal(t, true, captured["ForceNew"], "a distinct immutable publication must not be blocked as a textual lookalike")
}

func TestKataPublicationKeyDoesNotChangeWithCaptureTime(t *testing.T) {
	id := uuid.New()
	job := &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: t.TempDir()}
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
	c := storage.KataCompletion{Job: job, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail, At: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	resolve := func(context.Context, string, string) (kata.Binding, error) {
		return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
	}
	first, err := buildKataPublications(t.Context(), cfg, c, resolve)
	require.NoError(t, err)
	require.Len(t, first, 1)
	c.At = c.At.Add(time.Hour)
	second, err := buildKataPublications(t.Context(), cfg, c, resolve)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, first[0].OperationID, second[0].OperationID, "changed payload must conflict under the same publication key instead of creating a second issue")
}

func TestKataCommentsWithoutStructuredFindingsKeepsIssueBody(t *testing.T) {
	id := uuid.New()
	job := &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: t.TempDir()}
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "comments"}}
	got, err := buildKataPublications(t.Context(), cfg, storage.KataCompletion{Job: job, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail, Output: "Historical prose finding."}, func(context.Context, string, string) (kata.Binding, error) {
		return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Create.Body, "Historical prose finding.")
	assert.Empty(t, got[0].Entries, "prose must not manufacture finding identities")
}

func TestKataFailedManualRunsKeepDistinctFrozenPublications(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "test")
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.failed"}}
	tc.DB.SetKataHandoffBuilder(func(ctx context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
		return buildKataPublications(ctx, cfg, c, func(context.Context, string, string) (kata.Binding, error) {
			return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
		})
	})
	// An automatic retry is still the same manual run.
	beforeRetry := job
	retried, err := tc.DB.RetryJob(job.ID, testWorkerID, maxRetries, 0)
	require.NoError(t, err)
	require.True(t, retried)
	job, err = tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, storage.KataFailureRun(beforeRetry), storage.KataFailureRun(job))
	assert.NotEqual(t, beforeRetry.StartedAtRaw, job.StartedAtRaw)
	_, events := tc.Broadcaster.Subscribe("")
	var first storage.KataHandoff
	for i, diagnostic := range []string{"first synthetic failure", "second synthetic failure"} {
		_, err := tc.DB.Exec(`UPDATE review_jobs SET retry_count=? WHERE id=?`, maxRetries, job.ID)
		require.NoError(t, err)
		tc.Pool.failOrRetry(testWorkerID, job, "test", diagnostic)
		current, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusFailed, current.Status)
		require.Len(t, events, 1)
		assert.Equal(t, "review.failed", (<-events).Type)
		records, err := tc.DB.KataHandoffs(t.Context(), job.ID)
		require.NoError(t, err)
		require.Len(t, records, i+1)
		if i == 0 {
			first = records[0]
			require.NoError(t, tc.DB.ReenqueueJob(job.ID, storage.ReenqueueOpts{}))
			rerun, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, rerun)
			assert.Equal(t, job.UUID, rerun.UUID)
			assert.NotEqual(t, job.EnqueuedAt, rerun.EnqueuedAt)
			job = rerun
		} else {
			byID := map[string]storage.KataHandoff{}
			for _, r := range records {
				byID[r.OperationID] = r
			}
			assert.Equal(t, first, byID[first.OperationID])
			delete(byID, first.OperationID)
			for _, r := range byID {
				assert.Contains(t, r.Request.Create.Body, diagnostic)
			}
		}
	}
	// Repeated explicit capture of the same terminal run cannot duplicate it.
	require.NoError(t, tc.DB.CaptureKataHandoffs(t.Context(), job.ID))
	records, err := tc.DB.KataHandoffs(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Len(t, records, 2)
}

func TestKataPublicationUsesValidatedCallerCheckout(t *testing.T) {
	for _, mode := range []string{"summary", "no-hook", "unrelated", "gone"} {
		t.Run(mode, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			require.NoError(t, config.SaveRepoConfigTo(filepath.Join(repo.Root, ".roborev.toml"), &config.RepoConfig{Hooks: []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "comments"}}}))
			require.NoError(t, os.WriteFile(filepath.Join(repo.Root, ".kata.toml"), []byte("[project]\nname = \"main-project\"\n"), 0o600))
			checkout := filepath.Join(t.TempDir(), "caller")
			wantProject, wantPolicy := "worktree-project", "summary"
			switch mode {
			case "summary", "no-hook":
				repo.RunGit("worktree", "add", "--detach", checkout, "HEAD")
			case "unrelated":
				checkout = testutil.NewTestRepoWithCommit(t).Root
				wantProject, wantPolicy = "main-project", "comments"
			case "gone":
				wantProject, wantPolicy = "main-project", "comments"
			}
			if mode != "gone" {
				repoConfig := &config.RepoConfig{}
				if mode != "no-hook" {
					repoConfig.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
				}
				require.NoError(t, config.SaveRepoConfigTo(filepath.Join(checkout, ".roborev.toml"), repoConfig))
				require.NoError(t, os.WriteFile(filepath.Join(checkout, ".kata.toml"), []byte("[project]\nname = \"worktree-project\"\n"), 0o600))
			}
			cfg := config.DefaultConfig()
			cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "default-project"}
			id, reviewID := uuid.New(), uuid.New()
			const secret = "WORKTREE_PRIVATE_SENTINEL"
			completion := storage.KataCompletion{Job: &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: repo.Root, WorktreePath: checkout}, ReviewUUID: reviewID.String(), Verdict: storage.VerdictFail, Output: secret, Snapshots: []storage.FindingSnapshot{{Selector: "f:example", ReviewUUID: reviewID, DocumentHash: strings.Repeat("a", 64), Finding: structuredreview.Finding{Severity: "high", Problem: secret, Fix: secret}}}}
			var resolved []string
			requests, err := buildKataPublications(t.Context(), cfg, completion, func(_ context.Context, _ string, project string) (kata.Binding, error) {
				resolved = append(resolved, project)
				return kata.Binding{Project: project, ProjectUID: project + "-uid"}, nil
			})
			require.NoError(t, err)
			if mode == "no-hook" {
				assert.Empty(t, requests, "the main checkout cannot authorize worktree disclosure")
				assert.Empty(t, resolved)
				return
			}
			require.Len(t, requests, 1)
			assert.Equal(t, []string{wantProject}, resolved)
			assert.Equal(t, wantProject, requests[0].Project)
			assert.Equal(t, wantProject+"-uid", requests[0].ProjectUID)
			assert.Equal(t, wantPolicy, requests[0].Policy)
			payload, err := json.Marshal(requests[0])
			require.NoError(t, err)
			if wantPolicy == "summary" {
				assert.NotContains(t, string(payload), secret)
			} else {
				assert.Contains(t, string(payload), secret)
			}
		})
	}
}

func TestKataIdenticalHooksCaptureOneResolutionAtomically(t *testing.T) {
	for _, terminal := range []string{"completed", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			repo := t.TempDir()
			hook := config.HookConfig{Type: "kata", Event: "review.*", Findings: "summary", Project: "reviews"}
			require.NoError(t, config.SaveRepoConfigTo(filepath.Join(repo, ".roborev.toml"), &config.RepoConfig{Hooks: []config.HookConfig{hook}}))
			cfg := config.DefaultConfig()
			cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example"}
			cfg.Hooks = []config.HookConfig{hook}
			lookups := 0
			db.SetKataHandoffBuilder(func(ctx context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
				return buildKataPublications(ctx, cfg, c, func(context.Context, string, string) (kata.Binding, error) {
					lookups++
					if lookups == 1 {
						return kata.Binding{}, errors.New("initial outage")
					}
					return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
				})
			})
			job := createTestJob(t, db, repo, "abc123", "test")
			_, err := db.ClaimJob("worker")
			require.NoError(t, err)
			wantStatus := storage.JobStatusDone
			if terminal == "completed" {
				require.NoError(t, db.CompleteJob(job.ID, "test", `{"schema_version":2,"summary":"Synthetic review","verdict":"fail","findings":[{"severity":"high","problem":"Synthetic finding","fix":"Check the boundary","location":null}]}`))
			} else {
				updated, err := db.FailJob(job.ID, "worker", "synthetic failure")
				require.NoError(t, err)
				require.True(t, updated)
				wantStatus = storage.JobStatusFailed
			}
			current, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, wantStatus, current.Status)
			records, err := db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Empty(t, records[0].Request.ProjectUID, "the first frozen resolution remains pending")
			assert.Equal(t, "reviews", records[0].Request.Project)
			assert.Equal(t, 1, lookups)
		})
	}
}

func TestKataUnboundHooksDoNotCapturePublication(t *testing.T) {
	for _, terminal := range []string{"completed", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			repo := t.TempDir()
			_, err := kata.NewCLIClient(repo).Binding(t.Context())
			require.ErrorIs(t, err, kata.ErrNoBinding)
			cfg := config.DefaultConfig()
			cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example"}
			cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*"}}
			resolved := false
			db.SetKataHandoffBuilder(func(ctx context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
				return buildKataPublications(ctx, cfg, c, func(context.Context, string, string) (kata.Binding, error) {
					resolved = true
					return kata.Binding{}, errors.New("no project selected")
				})
			})
			job := createTestJob(t, db, repo, "abc123", "test")
			_, err = db.ClaimJob("worker")
			require.NoError(t, err)
			wantStatus := storage.JobStatusDone
			if terminal == "completed" {
				require.NoError(t, db.CompleteJob(job.ID, "test", findingsReviewDocument))
			} else {
				updated, err := db.FailJob(job.ID, "worker", "synthetic failure")
				require.NoError(t, err)
				require.True(t, updated)
				wantStatus = storage.JobStatusFailed
			}
			require.NoError(t, db.CaptureKataHandoffs(t.Context(), job.ID))
			current, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, wantStatus, current.Status)
			records, err := db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Empty(t, records, "an unconfigured destination must not leave retryable work")
			assert.False(t, resolved, "no destination means there is nothing to resolve")
		})
	}
}

func TestKataUnreadableCredentialCaptureCanBeReauthorized(t *testing.T) {
	for _, terminal := range []string{"completed", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			cfg := config.DefaultConfig()
			keyFile := filepath.Join(t.TempDir(), "token")
			cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", DefaultProject: "reviews", APIKeyFile: keyFile}
			cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
			db.SetKataHandoffBuilder(func(ctx context.Context, c storage.KataCompletion) ([]katapublish.Request, error) {
				return buildKataPublications(ctx, cfg, c, func(context.Context, string, string) (kata.Binding, error) {
					return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
				})
			})
			job := createTestJob(t, db, t.TempDir(), "abc123", "test")
			_, err := db.ClaimJob("worker")
			require.NoError(t, err)
			if terminal == "completed" {
				require.NoError(t, db.CompleteJob(job.ID, "test", findingsReviewDocument))
			} else {
				updated, err := db.FailJob(job.ID, "worker", "synthetic failure")
				require.NoError(t, err)
				require.True(t, updated)
			}
			records, err := db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, records, 1)
			assert.Equal(t, "blocked", records[0].Request.Policy)
			require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-restored-key"), 0o600))
			require.NoError(t, db.CaptureKataHandoffs(t.Context(), job.ID))
			records, err = db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, records, 2)
			var publications []katapublish.Request
			for _, record := range records {
				if record.Request.Policy != "blocked" {
					publications = append(publications, record.Request)
				}
			}
			require.Len(t, publications, 1)
			connection, err := kataPublicationConnection(cfg.Integrations.Kata, publications[0])
			require.NoError(t, err)
			assert.Equal(t, "synthetic-restored-key", connection.APIKey)
			require.NoError(t, db.CaptureKataHandoffs(t.Context(), job.ID))
			records, err = db.KataHandoffs(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Len(t, records, 2, "repeated explicit capture must retain one repaired publication")
		})
	}
}

func TestKataCaptureFreezesTheCredentialReadBeforeResolution(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-original-key"), 0o600))
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example", APIKeyFile: keyFile, DefaultProject: "reviews"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.*", Findings: "summary"}}
	id := uuid.New()
	c := storage.KataCompletion{Job: &storage.ReviewJob{ID: 1, UUID: &id, JobType: storage.JobTypeReview, RepoPath: t.TempDir()}, ReviewUUID: uuid.New().String(), Verdict: storage.VerdictFail}
	requests, err := buildKataPublications(t.Context(), cfg, c, func(context.Context, string, string) (kata.Binding, error) {
		require.NoError(t, os.Remove(keyFile))
		return kata.Binding{Project: "reviews", ProjectUID: "project-1"}, nil
	})
	require.NoError(t, err)
	require.Len(t, requests, 1)
	req := requests[0]
	_, err = kataPublicationConnection(cfg.Integrations.Kata, req)
	require.ErrorContains(t, err, "credential unavailable")
	assert.NotContains(t, err.Error(), keyFile)
	require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-replacement-key"), 0o600))
	_, err = kataPublicationConnection(cfg.Integrations.Kata, req)
	require.ErrorContains(t, err, "credential changed")
	require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-original-key"), 0o600))
	connection, err := kataPublicationConnection(cfg.Integrations.Kata, req)
	require.NoError(t, err)
	assert.Equal(t, "synthetic-original-key", connection.APIKey)
	assert.Empty(t, connection.APIKeyFile)
}
