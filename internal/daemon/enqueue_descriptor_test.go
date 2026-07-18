package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// enqueueViaHTTP posts an enqueue request through the real HTTP handler and
// decodes the created job. It asserts a 201 so the regression tests fail loudly
// if the refactor changes the response shape or status.
func enqueueViaHTTP(t *testing.T, server *Server, body EnqueueRequest) storage.ReviewJob {
	t.Helper()

	w := enqueueRaw(t, server, body)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	return job
}

func enqueueRaw(
	t *testing.T, server *Server, body EnqueueRequest,
) *httptest.ResponseRecorder {
	t.Helper()
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", body)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// TestEnqueueSingleCommitUnchanged pins the single-commit enqueue path: the
// stored job must carry CommitID, the frozen SHA, a non-empty PatchID, and the
// commit subject, with the resolved agent/model/reasoning preserved verbatim.
func TestEnqueueSingleCommitUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	assert.Equal(storage.JobTypeReview, stored.JobType)
	assert.Equal(sha, stored.GitRef)
	assert.NotZero(stored.CommitIDValue(), "single-commit job must reference a commit row")
	assert.NotEmpty(stored.PatchID, "single-commit job must record a patch id")
	assert.Equal("test", stored.Agent)
	assert.NotEmpty(stored.Reasoning, "reasoning must be resolved")
	assert.Empty(stored.Prompt, "single-commit review must not store a prompt")

	// CommitSubject is set on the returned job (not persisted on review_jobs),
	// so assert against the HTTP response value.
	assert.Equal("add a", job.CommitSubject)
}

// TestEnqueueDirtyUnchanged pins the dirty enqueue path: the stored job must be
// classified as dirty and preserve the supplied diff content.
func TestEnqueueDirtyUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	diff := "diff --git a/x b/x\n+change\n"
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:    repo.Path(),
		GitRef:      "dirty",
		Agent:       "test",
		DiffContent: diff,
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeDirty, stored.JobType)
	assert.Equal("dirty", stored.GitRef)
	assert.NotZero(stored.CommitIDValue(), "dirty job stores base HEAD for session reuse")

	// GetJobByID does not hydrate diff_content; the worker reads it via ClaimJob,
	// so verify the stored diff survives through the worker's view.
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NotNil(t, claimed.DiffContent)
	assert.Equal(diff, *claimed.DiffContent)
}

func TestEnqueueDirtyRejectsEmptyNonMetadataDiff(t *testing.T) {
	server, _, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:   repo.Path(),
		GitRef:     "dirty",
		Agent:      "test",
		DirtyFiles: []string{".cache/generated.bin"},
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "diff_content required")
}

func TestEnqueueDirtyMetadataOnlyPersistsDirtyFiles(t *testing.T) {
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:   repo.Path(),
		GitRef:     "dirty",
		Agent:      "test",
		DirtyFiles: []string{"go.sum"},
	})

	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	assert.Equal(t, storage.JobTypeDirty, claimed.JobType)
	assert.Nil(t, claimed.DiffContent)
	assert.Equal(t, []string{"go.sum"}, claimed.DirtyFiles)
	assert.False(t, claimed.PromptPrebuilt, "dirty metadata should be rebuilt by the worker")
}

// TestEnqueueRangeUnchanged pins the range enqueue path: the symbolic
// "<a>..<b>" request is frozen to "<sha>..<sha>" and classified as a range.
func TestEnqueueRangeUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	firstSHA := repo.CommitFile("a.txt", "a", "add a")
	secondSHA := repo.CommitFile("b.txt", "b", "add b")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD~1..HEAD",
		Agent:    "test",
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	assert.Equal(storage.JobTypeRange, stored.JobType)
	assert.Equal(firstSHA+".."+secondSHA, stored.GitRef)
	assert.Zero(stored.CommitIDValue(), "range job must not reference a single commit row")
}

// TestEnqueuePromptJobUnchanged pins the stored-prompt enqueue path: the
// finding-driven regression proving Prompt/OutputPrefix/Agentic/Label survive
// the refactor and the prompt is not flagged prebuilt.
func TestEnqueuePromptJobUnchanged(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:     repo.Path(),
		GitRef:       "task-label",
		Agent:        "test",
		CustomPrompt: "do X",
		OutputPrefix: "P",
		Agentic:      true,
		JobType:      storage.JobTypeTask,
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeTask, stored.JobType)
	assert.Equal("do X", stored.Prompt)
	assert.True(stored.Agentic)
	// Label drives the git_ref display value for task jobs.
	assert.Equal("task-label", stored.GitRef)
	assert.Zero(stored.CommitIDValue(), "prompt job must not reference a commit row")

	// GetJobByID omits output_prefix / prompt_prebuilt; the worker reads them via
	// ClaimJob, so verify them through the worker's view.
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	assert.Equal("P", claimed.OutputPrefix)
	assert.False(claimed.PromptPrebuilt, "humaEnqueue never marks prompts prebuilt")
}

// TestEnqueueResponseUnchanged pins the HTTP response shape: a bare JSON
// object (status 201) wire-compatible with storage.ReviewJob, with the
// launch UUID always present and no undeclared extra fields.
func TestEnqueueResponseUnchanged(t *testing.T) {
	server, _, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.True(t, strings.HasPrefix(strings.TrimSpace(w.Body.String()), "{"),
		"response must be a bare JSON object, not a wrapper")

	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "$schema",
		"created enqueue response must not carry huma's injected $schema link; "+
			"the EnqueueCreatedResponse schema declares additionalProperties:false")

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	assert.Positive(t, job.ID)
	assert.NotEmpty(t, job.UUID, "created enqueue response must emit its launch UUID")
	assert.Equal(t, "test", job.Agent)
	assert.Equal(t, storage.JobStatusQueued, job.Status)
}

// TestEnqueueExcludedCommitSkips pins the single-commit skip path: when the HEAD
// commit message matches an excluded pattern, buildTargetDescriptor returns the
// 200 Skipped *RawJSONOutput early return instead of a job. This is the
// skip-branch the descriptor refactor must preserve, distinct from the 201 paths.
func TestEnqueueExcludedCommitSkips(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", "excluded_commit_patterns = [\"skipme\"]\n")
	repo.CommitFile("a.txt", "a", "skipme: trivial change")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp EnqueueSkippedResponse
	testutil.DecodeJSON(t, w, &resp)
	assert.True(resp.Skipped, "excluded commit must report skipped")
	assert.Contains(resp.Reason, "excluded pattern")

	// A skipped enqueue must not create any job row.
	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Empty(jobs, "skipped enqueue must not create a job")
}

func TestEnqueuePostCommitDuplicateSkips(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")
	body := EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
		Source:   storage.JobSourcePostCommit,
	}
	subID, eventCh := server.broadcaster.Subscribe("")
	defer server.broadcaster.Unsubscribe(subID)

	first := enqueueRaw(t, server, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	require.Len(t, eventCh, 1, "created enqueue should broadcast job.enqueued")
	<-eventCh
	beforeActivity := 0
	for _, entry := range server.activityLog.Recent() {
		if entry.Event == "job.enqueued" {
			beforeActivity++
		}
	}

	second := enqueueRaw(t, server, body)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var skipped EnqueueSkippedResponse
	testutil.DecodeJSON(t, second, &skipped)
	assert.True(t, skipped.Skipped)
	assert.Contains(t, skipped.Reason, "matching post-commit job")

	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	assert.Empty(t, eventCh, "duplicate enqueue must not broadcast an event")
	afterActivity := 0
	for _, entry := range server.activityLog.Recent() {
		if entry.Event == "job.enqueued" {
			afterActivity++
		}
	}
	assert.Equal(t, beforeActivity, afterActivity)
}

func TestEnqueuePostCommitDoesNotSuppressExplicitReview(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")
	body := EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD", Agent: "test",
		Source: storage.JobSourcePostCommit,
	}
	require.Equal(t, http.StatusCreated, enqueueRaw(t, server, body).Code)

	body.Source = ""
	require.Equal(t, http.StatusCreated, enqueueRaw(t, server, body).Code)
	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Len(t, jobs, 2)
}

func TestEnqueuePostCommitAllowsCanceledReplacement(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")
	body := EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD", Agent: "test",
		Source: storage.JobSourcePostCommit,
	}
	first := enqueueRaw(t, server, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var job storage.ReviewJob
	testutil.DecodeJSON(t, first, &job)
	require.NoError(t, db.CancelJob(job.ID))

	second := enqueueRaw(t, server, body)
	require.Equal(t, http.StatusCreated, second.Code, second.Body.String())
}

func TestEnqueuePostCommitConcurrentRequestsCreateOneJob(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")
	payload, err := json.Marshal(EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD", Agent: "test",
		Source: storage.JobSourcePostCommit,
	})
	require.NoError(t, err)

	const requestCount = 8
	start := make(chan struct{})
	statuses := make(chan int, requestCount)
	for range requestCount {
		go func() {
			<-start
			req := httptest.NewRequest(
				http.MethodPost, "/api/enqueue", strings.NewReader(string(payload)),
			)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			statuses <- w.Code
		}()
	}
	close(start)

	created := 0
	skipped := 0
	unexpected := 0
	for range requestCount {
		switch status := <-statuses; status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			skipped++
		default:
			unexpected++
		}
	}
	assert.Equal(t, 1, created)
	assert.Equal(t, requestCount-1, skipped)
	assert.Zero(t, unexpected)

	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
}
