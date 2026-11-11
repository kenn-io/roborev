package daemon

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestKataSelectionResolvesAmbiguousDestinations(t *testing.T) {
	dir := testutil.NewTestRepo(t).Root
	writeBinding := func(name string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("version = 1\n[project]\nname = \""+name+"\"\n"), 0o600))
	}
	fake := katafake.New(t)
	cfg := kataConnectionConfig(fake)
	job := &storage.ReviewJob{JobType: storage.JobTypeReview}
	var aliasProject atomic.Int32
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/projects/resolve" {
			return false
		}
		var request struct {
			Alias gen.AliasInput `json:"alias"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			return true
		}
		project := fake.Projects[aliasProject.Load()]
		fake.Reply(w, gen.ProjectResolveBody{Project: project, Alias: gen.ProjectAlias{
			AliasIdentity: request.Alias.Identity, AliasKind: request.Alias.Kind,
			ID: 1, ProjectID: project.ID, CreatedAt: time.Unix(0, 0).UTC(),
		}})
		return true
	}
	cfg.Hooks = []config.HookConfig{
		{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"},
		{Type: "kata", Event: "review.completed", Findings: "comments"},
	}
	writeBinding("old-name")
	selected, err := selectKataPublication(t.Context(), cfg, job, dir)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, fake.Projects[0].UID, selected.resolvedUID)
	assert.Equal(t, "old-name", selected.seed.Project)
	assert.Equal(t, "local", selected.seed.AliasKind)
	emptyHint := selected.seed
	emptyHint.Project = ""
	uid, err := resolveKataSelection(t.Context(), cfg.Integrations.Kata, emptyHint)
	require.NoError(t, err)
	assert.Equal(t, fake.Projects[0].UID, uid)
	cfg.Hooks[0], cfg.Hooks[1] = cfg.Hooks[1], cfg.Hooks[0]
	reversed, err := selectKataPublication(t.Context(), cfg, job, dir)
	require.NoError(t, err)
	require.NotNil(t, reversed)
	assert.Equal(t, selected.seed, reversed.seed)
	cfg.Hooks[0], cfg.Hooks[1] = cfg.Hooks[1], cfg.Hooks[0]
	// A matching hint cannot prove identity: the alias now resolves elsewhere.
	writeBinding("reviews")
	aliasProject.Store(1)
	_, err = selectKataPublication(t.Context(), cfg, job, dir)
	require.ErrorContains(t, err, "conflicting")
	aliasProject.Store(0)
	for _, change := range []func(){
		func() { cfg.Hooks[1].Findings = "summary" },
		func() { cfg.Hooks[1].Labels = []string{"different"} },
		func() { cfg.Hooks[1].Priority = new(2) },
	} {
		cfg.Hooks[1] = config.HookConfig{Type: "kata", Event: "review.completed", Findings: "comments"}
		change()
		before := len(fake.AllRequests())
		_, err = selectKataPublication(t.Context(), cfg, job, dir)
		require.ErrorContains(t, err, "conflicting")
		assert.Len(t, fake.AllRequests(), before)
	}
	// A single selection never reads credentials or contacts Kata.
	cfg.Hooks = cfg.Hooks[1:]
	cfg.Hooks[0].Findings = "comments"
	cfg.Integrations.Kata.APIKey = ""
	cfg.Integrations.Kata.APIKeyFile = filepath.Join(dir, "missing-token")
	before := len(fake.AllRequests())
	selected, err = selectKataPublication(t.Context(), cfg, job, dir)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Empty(t, selected.resolvedUID)
	assert.Len(t, fake.AllRequests(), before)
	cfg.Hooks = append([]config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}, cfg.Hooks...)
	cfg.Hooks[1].Labels = nil
	cfg.Hooks[1].Priority = nil
	_, err = selectKataPublication(t.Context(), cfg, job, dir)
	require.ErrorContains(t, err, "resolve Kata publication hook")
}

func TestKataSelectionCoalescesAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Integrations: config.IntegrationsConfig{Kata: config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test", DefaultProject: "reviews"}}}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews", Labels: []string{"b", "a"}}, {Type: "kata", Event: "review.*", Findings: "comments", Project: "reviews", Labels: []string{"a", "b"}}}
	job := &storage.ReviewJob{JobType: storage.JobTypeReview}
	selection, err := selectKataPublication(t.Context(), cfg, job, "")
	require.NoError(t, err)
	require.NotNil(t, selection)
	assert.Equal(t, "reviews", selection.seed.Project)
	cfg.Hooks[1].Findings = "summary"
	selection, err = selectKataPublication(t.Context(), cfg, job, "")
	require.Error(t, err)
	assert.Nil(t, selection)
	job.Source = storage.JobSourceCI
	selection, err = selectKataPublication(t.Context(), cfg, job, "")
	require.NoError(t, err)
	assert.Nil(t, selection)
}

func TestKataHookFindingsValidation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Hooks = []config.HookConfig{{Type: "kata", Findings: "comments"}}
	require.NoError(t, cfg.Validate())
	cfg.Hooks[0].Findings = "summary"
	require.NoError(t, cfg.Validate())
	cfg.Hooks[0].Findings = "unexpected"
	require.ErrorContains(t, cfg.Validate(), "findings")
	cfg.Hooks[0] = config.HookConfig{Type: "webhook", Findings: "comments"}
	require.ErrorContains(t, cfg.Validate(), "findings")
	repo := &config.RepoConfig{Hooks: []config.HookConfig{{Type: "kata", Findings: "unexpected"}}}
	require.ErrorContains(t, repo.Validate(), "findings")
}

func TestKataSelectionEligibility(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "summary", Project: "reviews", Branches: []string{"main"}}}
	cases := []struct {
		name     string
		job      storage.ReviewJob
		eligible bool
	}{
		{"review", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main"}, true},
		{"range", storage.ReviewJob{JobType: storage.JobTypeRange, Branch: "main"}, true},
		{"dirty", storage.ReviewJob{JobType: storage.JobTypeDirty, Branch: "main"}, true},
		{"synthesis", storage.ReviewJob{JobType: storage.JobTypeSynthesis, Branch: "main"}, true},
		{"CI", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", Source: storage.JobSourceCI}, false},
		{"CI base", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", CIBaseBranch: "main"}, false},
		{"member", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", PanelRole: storage.PanelRoleMember}, false},
		{"rebuttal", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", Source: storage.JobSourceRebuttal}, false},
		{"goal", storage.ReviewJob{JobType: storage.JobTypeGoalReview, Branch: "main"}, false},
		{"task", storage.ReviewJob{JobType: storage.JobTypeTask, Branch: "main"}, false},
		{"compact", storage.ReviewJob{JobType: storage.JobTypeCompact, Branch: "main"}, false},
		{"branch", storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "other"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := selectKataPublication(t.Context(), cfg, &tc.job, "")
			require.NoError(t, err)
			assert.Equal(t, tc.eligible, selected != nil)
		})
	}
}

func TestKataSelectionSuppressesSuppliedRepoHooks(t *testing.T) {
	dir := t.TempDir()
	text := "[[hooks]]\ntype = \"kata\"\nevent = \"review.completed\"\nfindings = \"comments\"\nproject = \"repo\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(text), 0o600))
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test"}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "global"}}
	job := &storage.ReviewJob{JobType: storage.JobTypeReview}
	_, err := selectKataPublication(t.Context(), cfg, job, dir)
	require.ErrorContains(t, err, "conflicting")
	job.Source = storage.JobSourceReviewPrompt
	job.PanelMemberConfigJSON = "supplied"
	selected, err := selectKataPublication(t.Context(), cfg, job, dir)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "global", selected.seed.Project)
}

func TestKataCompletionCapturesUnknownVerdictFallback(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "fallback-review", "worker")
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test", APIKeyFile: filepath.Join(t.TempDir(), "missing-token")}
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}}
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.Pool.completeReviewResult(job, "test", storage.ReviewCompletion{Output: string(raw)}, tc.Pool.prepareKataPublication(t.Context(), "worker", job, tc.TmpDir)))
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	require.NotNil(t, record.Marker)
	assert.Equal(t, "comments", record.Marker.Policy)
}

func TestKataCompletionKeepsReviewWhenAmbiguousSelectionCannotResolve(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "offline-review", "worker")
	repo := testutil.NewTestRepo(t)
	repo.WriteFile(".kata.toml", "version = 1\n[project]\nname = \"reviews\"\n")
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.test", APIKeyFile: filepath.Join(t.TempDir(), "missing-token")}
	cfg.Hooks = []config.HookConfig{
		{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"},
		{Type: "kata", Event: "review.completed", Findings: "comments"},
	}
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.Pool.completeReviewResult(job, "test", storage.ReviewCompletion{Output: string(raw)}, tc.Pool.prepareKataPublication(t.Context(), "worker", job, repo.Root)))
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, record.Marker)
}

func TestKataInspectionProjectRequiresUnambiguousExplicitProject(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "other"}, {Type: "kata", Event: "review.completed", Findings: "summary", Project: "other"}}
	job := &storage.ReviewJob{JobType: storage.JobTypeReview}
	name, err := kataInspectionProject(cfg, job, "")
	require.NoError(t, err)
	assert.Equal(t, "other", name)
	cfg.Hooks[1].Project = "different"
	_, err = kataInspectionProject(cfg, job, "")
	require.ErrorContains(t, err, "ambiguous")
	cfg.Hooks = nil
	name, err = kataInspectionProject(cfg, job, "")
	require.NoError(t, err)
	assert.Empty(t, name)
}

func TestKataSynthesisSelectionDoesNotHoldAttemptGuard(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "blocked-selection", "worker")
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".kata.toml"), []byte("version = 1\n[project]\nname = \"reviews\"\n"), 0o600))
	fake := katafake.New(t)
	cfg := kataConnectionConfig(fake)
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "comments", Project: "reviews"}, {Type: "kata", Event: "review.completed", Findings: "comments"}}
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/projects/resolve" {
			return false
		}
		close(started)
		<-release
		fake.Reply(w, gen.ProjectResolveBody{Project: fake.Projects[0]})
		return true
	}
	go func() {
		defer close(done)
		tc.Pool.completeSynthesisContext(t.Context(), "worker", job, synthesisResult{review: reviewpkg.ReviewResult{Agent: "test", Output: "failed", Verdict: storage.VerdictFail, StructuredOutput: testutil.ReviewFixtureJSON("failed")}})
	}()
	t.Cleanup(func() { unblock(); <-done })
	waitStarted := func() error {
		select {
		case <-started:
			return nil
		case <-time.After(10 * time.Second): // External fake HTTP request.
			return errors.New("resolver did not start")
		}
	}
	require.NoError(t, waitStarted())
	locked := tc.Pool.attemptTransitionsMu.TryLock()
	require.True(t, locked)
	tc.Pool.attemptTransitionsMu.Unlock()
	deleted := make(chan error, 1)
	go func() { deleted <- tc.DB.DeleteRepoContext(context.WithoutCancel(t.Context()), tc.Repo.ID, true) }()
	waitDeleted := func() error {
		select {
		case err := <-deleted:
			return err
		case <-time.After(10 * time.Second): // External SQLite attempt lock.
			return errors.New("repository deletion blocked by resolver")
		}
	}
	require.NoError(t, waitDeleted())
	unblock()
	<-done
	_, err := tc.DB.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}
