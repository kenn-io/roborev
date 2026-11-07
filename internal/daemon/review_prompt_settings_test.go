package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSuppliedReviewSettingsIgnoreCheckoutPolicy(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	base := repo.CommitFile("file.txt", "before\n", "base")
	repo.CommitFile(".roborev.toml", "exclude_patterns = ['file.txt']\nreview_min_severity = 'high'\nmodel = 'local-model'\n", "local policy")
	head := repo.CommitFile("file.txt", "VISIBLE_CHANGE\n", "target")
	calls := 0
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-settings", ReviewFn: func(_ context.Context, root, ref, text string, _ io.Writer) (string, error) {
		calls++
		assert.Equal(base+".."+head, ref)
		matches, err := filepath.Glob(filepath.Join(root, ".roborev", "*", "roborev-snapshot-content.diff"))
		require.NoError(t, err)
		require.Len(t, matches, 1, text)
		data, err := os.ReadFile(matches[0])
		require.NoError(t, err)
		assert.Contains(string(data), "+VISIBLE_CHANGE")
		return `{"schema_version":3,"summary":"Minor defect","verdict":"fail","findings":[{"severity":"low","kind":"defect","confidence":0.8,"problem":"Missing check.","fix":"Add check.","file":null,"start_line":null,"end_line":null}]}`, nil
	}})
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": base + ".." + head, "agent": "supplied-settings", "model": "supplied-model",
		"review_prompt": "Review the diff at " + prompt.DiffFilePathPlaceholder, "review_settings": map[string]any{},
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	for run := range 2 {
		if run == 1 {
			// Invalid local policy cannot replace, hide, or prevent a frozen rerun.
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("not valid TOML ["), 0o600))
			_, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: job.ID}})
			require.NoError(t, err)
		}
		claimed, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		assert.Equal("supplied-model", claimed.Model)
		server.workerPool.processJob(testWorkerID, claimed)
		stored, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
		result, err := db.GetReviewByJobID(job.ID)
		require.NoError(t, err)
		require.NotNil(t, result.VerdictBool)
		assert.Equal(0, *result.VerdictBool)
	}
	assert.Equal(2, calls)
}

func TestSuppliedReviewSettingsRetainExecutionAcrossImport(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	server.workerPool.retryBackoff = 0
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target\n", "target")
	primaryCalls, backupCalls := 0, 0
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-primary", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		primaryCalls++
		if primaryCalls == 1 {
			return "", errors.New("connection reset by peer")
		}
		return "", errors.New("context window exceeded")
	}})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-backup", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		backupCalls++
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: head, Agent: "supplied-primary", Model: "primary-model", Reasoning: "high",
		ReviewPrompt: "Review the change.", ReviewSettings: &SuppliedReviewSettings{BackupAgent: "supplied-backup", BackupModel: "backup-model"},
	})
	// Re-reading this config must not replace the selected agent or backup.
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("invalid TOML ["), 0o600))
	for _, want := range []string{"supplied-primary", "supplied-primary", "supplied-backup"} {
		claimed, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		require.Equal(t, want, claimed.Agent)
		if want == "supplied-backup" {
			assert.Equal("backup-model", claimed.Model)
		}
		server.workerPool.processJob(testWorkerID, claimed)
	}
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
	assert.Equal(2, primaryCalls)
	assert.Equal(1, backupCalls)
	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	exported, err := db.GetJobsToSync(machineID, 10)
	require.NoError(t, err)
	require.Len(t, exported, 1)
	e := exported[0]
	server, db, _ = newTestServer(t)
	localRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(localRepo.ID, e.CommitSHA, e.CommitAuthor, e.CommitSubject, e.CommitTimestamp)
	require.NoError(t, err)
	require.NoError(t, db.UpsertPulledJob(storage.PulledJob{
		UUID: e.UUID, GitRef: e.GitRef, Agent: e.Agent, Model: e.Model, Source: e.Source, Prompt: e.Prompt, JobType: e.JobType,
		Status: e.Status, EnqueuedAt: e.EnqueuedAt, FinishedAt: e.FinishedAt, UpdatedAt: e.UpdatedAt, SourceMachineID: e.SourceMachineID,
		PanelMemberConfigJSON: e.PanelMemberConfigJSON,
	}, localRepo.ID, &commit.ID))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, e.UUID).Scan(&job.ID))
	_, err = server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: job.ID}})
	require.NoError(t, err)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal("supplied-primary", claimed.Agent)
	assert.Equal("primary-model", claimed.Model)
	assert.Equal("high", claimed.Reasoning)
	assert.Equal("supplied-backup", claimed.BackupAgent)
	server.workerPool.processJob(testWorkerID, claimed)
	claimed, err = db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, "supplied-backup", claimed.Agent)
	server.workerPool.processJob(testWorkerID, claimed)
	stored, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
	assert.Equal(3, primaryCalls)
	assert.Equal(2, backupCalls)
}

func TestSuppliedReviewSettingsEnforceToolPolicy(t *testing.T) {
	t.Parallel()
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile(".roborev.toml", "invalid TOML [", "target")
	calls := 0
	agent.RegisterForTest(t, &strictWorkerTestAgent{
		structuredWorkerTestAgent: &structuredWorkerTestAgent{name: "supplied-strict-tools", result: jsontext.Value(testutil.ReviewFixtureJSON("No issues found."))},
		onReview:                  func(path string) { calls++; assert.FileExists(t, path) },
	})
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: head, Agent: "supplied-strict-tools", ReviewPrompt: "Review the change.",
		ReviewSettings: &SuppliedReviewSettings{Tools: &config.ReviewToolPolicy{Mode: "read-only"}},
	})
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	server.workerPool.processJob(testWorkerID, claimed)
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
	assert.Equal(t, 1, calls)
	_, err = server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: job.ID, Agent: "test"}})
	require.ErrorContains(t, err, "cannot change agents")
}

func TestSuppliedReviewSettingsRejectInvalidRequests(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, prompt string
		settings     SuppliedReviewSettings
		want         string
	}{
		{name: "missing prompt", want: "requires review_prompt"},
		{name: "dynamic context", prompt: prompt.PriorRangeReviewsFilePathPlaceholder, want: "prior review context"},
		{name: "tool mode", prompt: "Review.", settings: SuppliedReviewSettings{Tools: &config.ReviewToolPolicy{Mode: "write"}}, want: "read-only"},
		{name: "backup pairing", prompt: "Review.", settings: SuppliedReviewSettings{BackupModel: "model"}, want: "backup_model requires backup_agent"},
		{name: "unsupported tools", prompt: "Review.", settings: SuppliedReviewSettings{Tools: &config.ReviewToolPolicy{Mode: "read-only"}}, want: "does not support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target", "target")
			w := enqueueRaw(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "test", ReviewPrompt: tc.prompt, ReviewSettings: &tc.settings})
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.want)
			jobs, err := db.ListJobs("", "", 10, 0)
			require.NoError(t, err)
			assert.Empty(t, jobs)
		})
	}
}

func TestSuppliedReviewSettingsIgnoreCommitExclusions(t *testing.T) {
	t.Parallel()
	for _, rangeReview := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "range"}[rangeReview], func(t *testing.T) {
			server, _, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			base := repo.CommitFile(".roborev.toml", "excluded_commit_patterns = ['target']\n", "policy")
			head := repo.CommitFile("file.txt", "change", "target")
			ref := head
			if rangeReview {
				ref = base + ".." + head
			}
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: ref, Agent: "test", ReviewPrompt: "Review.", ReviewSettings: &SuppliedReviewSettings{}})
			assert.Equal(t, ref, job.GitRef)
		})
	}
}

func TestSuppliedReviewSettingsControlBinaryEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		enabled   bool
		excludes  []string
		wantFiles int
	}{
		{name: "enabled", enabled: true, wantFiles: 1},
		{name: "disabled"},
		{name: "excluded", enabled: true, excludes: []string{"sample.bin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			repo.CommitFile(".roborev.toml", "exclude_patterns = ['sample.bin']\nsnapshot_dir = 'local-snapshots'\nmax_prompt_size = 1\nauto_close_passing_reviews = true\n", "local policy")
			head := repo.CommitFile("sample.bin", "\x00COMMITTED_BYTES", "target")
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), "sample.bin"), []byte("\x00LATER_BYTES"), 0o600))
			name := "supplied-binary-" + tc.name
			calls := 0
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, root, ref, text string, _ io.Writer) (string, error) {
				calls++
				assert.Contains(t, text, "Review the binary.", "repository prompt threshold cannot change transport")
				matches, err := filepath.Glob(filepath.Join(root, ".roborev", "*", "*.bin"))
				require.NoError(t, err)
				require.Len(t, matches, tc.wantFiles, text)
				if len(matches) > 0 {
					data, err := os.ReadFile(matches[0])
					require.NoError(t, err)
					assert.Equal(t, "\x00COMMITTED_BYTES", string(data))
				}
				return string(testutil.ReviewFixtureJSON("No issues found.")), nil
			}})
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: name, ReviewPrompt: "Review the binary.", ReviewSettings: &SuppliedReviewSettings{BinaryEvidence: tc.enabled, ExcludePatterns: tc.excludes}})
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			server.workerPool.processJob(testWorkerID, claimed)
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.False(t, review.Closed)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestSuppliedReviewSettingsEmptyBackupStaysEmpty(t *testing.T) {
	t.Parallel()
	server, db, _ := newTestServer(t)
	server.configWatcher.Config().DefaultBackupAgent = "test"
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile(".roborev.toml", "backup_agent = 'test'\n", "target")
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-no-backup", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return "", errors.New("context window exceeded")
	}})
	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "supplied-no-backup", ReviewPrompt: "Review.", ReviewSettings: &SuppliedReviewSettings{}})
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	server.workerPool.processJob(testWorkerID, claimed)
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, stored.Status)
	assert.Equal(t, "supplied-no-backup", stored.Agent)
}

func TestSuppliedReviewSettingsSameAgentKeepsPrimaryModel(t *testing.T) {
	t.Parallel()
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target", "target")
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: head, Agent: "test", Model: "primary-model", ReviewPrompt: "Review.",
		ReviewSettings: &SuppliedReviewSettings{BackupAgent: "test", BackupModel: "backup-model"},
	})
	assert.Equal(t, "primary-model", job.Model)
	assert.Empty(t, job.BackupAgent)
}
