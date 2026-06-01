// Package testutil provides shared test utilities for roborev tests.
package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

const (
	GitUserName  = "Test"
	GitUserEmail = "test@test.com"
)

// Chdir changes the working directory to the repo root and returns a
// restore function. The caller should defer the returned function.
func (r *TestRepo) Chdir() func() {
	r.t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chdir(r.Root); err != nil {
		r.t.Fatal(err)
	}
	return func() {
		if err := os.Chdir(orig); err != nil {
			r.t.Fatal(err)
		}
	}
}

// WriteHook writes a post-commit hook with the given content.
func (r *TestRepo) WriteHook(content string) {
	r.t.Helper()
	r.WriteNamedHook("post-commit", content)
}

// WriteNamedHook writes a hook with the given name and content.
func (r *TestRepo) WriteNamedHook(name, content string) {
	r.t.Helper()
	if err := os.MkdirAll(r.HooksDir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	hookPath := filepath.Join(r.HooksDir, name)
	if err := os.WriteFile(hookPath, []byte(content), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// GetHookPath returns the path to a specific hook in the test repository.
func (r *TestRepo) GetHookPath(name string) string {
	r.t.Helper()
	return filepath.Join(r.HooksDir, name)
}

// RemoveHooksDir removes the .git/hooks directory.
func (r *TestRepo) RemoveHooksDir() {
	r.t.Helper()
	if err := os.RemoveAll(r.HooksDir); err != nil {
		r.t.Fatal(err)
	}
}

type pathMode int

const (
	pathPrepend pathMode = iota
	pathIsolate
)

func executableName(binName string) string {
	if runtime.GOOS == "windows" && filepath.Ext(binName) == "" {
		return binName + ".bat"
	}
	return binName
}

func mockScriptInPath(t *testing.T, binName, scriptContent string, mode pathMode) func() {
	t.Helper()

	tmpBin := t.TempDir()

	path := filepath.Join(tmpBin, executableName(binName))
	content := []byte(scriptContent)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	switch mode {
	case pathIsolate:
		os.Setenv("PATH", tmpBin)
	default:
		os.Setenv("PATH", tmpBin+string(os.PathListSeparator)+origPath)
	}

	return func() {
		os.Setenv("PATH", origPath)
	}
}

func mockExitCodeExecutable(t *testing.T, binName string, exitCode int, mode pathMode) func() {
	t.Helper()

	var scriptContent string
	if runtime.GOOS == "windows" {
		scriptContent = fmt.Sprintf("@exit /b %d\r\n", exitCode)
	} else {
		scriptContent = fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	}

	return mockScriptInPath(t, binName, scriptContent, mode)
}

// MockExecutable creates a fake executable in PATH that exits with the given code.
// Returns a cleanup function.
func MockExecutable(t *testing.T, binName string, exitCode int) func() {
	t.Helper()
	return mockExitCodeExecutable(t, binName, exitCode, pathPrepend)
}

// MockExecutableIsolated creates a fake executable in a new directory and sets PATH to ONLY that directory.
// Returns a cleanup function.
func MockExecutableIsolated(t *testing.T, binName string, exitCode int) func() {
	t.Helper()
	return mockExitCodeExecutable(t, binName, exitCode, pathIsolate)
}

// MockBinaryInPath creates a fake executable in PATH and returns a cleanup function.
func MockBinaryInPath(t *testing.T, binName, scriptContent string) func() {
	t.Helper()
	return mockScriptInPath(t, binName, scriptContent, pathPrepend)
}

// OpenTestDB creates a test database in a temporary directory.
// The database is automatically closed when the test completes.
func OpenTestDB(t *testing.T) *storage.DB {
	t.Helper()

	db, _ := OpenTestDBWithDir(t)
	return db
}

// OpenTestDBWithDir creates a test database and returns both the DB and the
// temporary directory path. Useful when tests need to create repos or other
// files in the same directory. The database is automatically closed when
// the test completes.
func OpenTestDBWithDir(t *testing.T) (*storage.DB, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open test DB: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db, tmpDir
}

// AssertStatusCode checks that the response has the expected HTTP status code.
// On failure, it reports the response body for debugging.
func AssertStatusCode(t *testing.T, w *httptest.ResponseRecorder, expected int) {
	t.Helper()

	if w.Code != expected {
		t.Errorf("Expected status %d, got %d: %s", expected, w.Code, w.Body.String())
	}
}

// CreateTestRepo creates a test repository in the database.
// Uses the test's temp directory as the repo path.
func CreateTestRepo(t *testing.T, db *storage.DB) *storage.Repo {
	t.Helper()

	tmpDir := t.TempDir()
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		t.Fatalf("GetOrCreateRepo failed: %v", err)
	}

	return repo
}

// CreateTestJobs creates the specified number of test jobs in a repository.
// Each job is created with a unique SHA (sha0, sha1, etc.) and the specified agent.
// Returns the created jobs in order.
func CreateTestJobs(t *testing.T, db *storage.DB, repo *storage.Repo, count int, agent string) []*storage.ReviewJob {
	t.Helper()

	jobs := make([]*storage.ReviewJob, 0, count)

	for i := range count {
		sha := fmt.Sprintf("sha%d", i)
		commit, err := db.GetOrCreateCommit(repo.ID, sha, "Test Author", fmt.Sprintf("Test commit %d", i), time.Now())
		if err != nil {
			t.Fatalf("GetOrCreateCommit failed: %v", err)
		}

		job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Agent: agent})
		if err != nil {
			t.Fatalf("EnqueueJob failed: %v", err)
		}

		jobs = append(jobs, job)
	}

	return jobs
}

// CreateTestJobWithSHA creates a single test job with a specific SHA.
func CreateTestJobWithSHA(t *testing.T, db *storage.DB, repo *storage.Repo, sha, agent string) *storage.ReviewJob {
	t.Helper()

	commit, err := db.GetOrCreateCommit(repo.ID, sha, "Test Author", "Test commit", time.Now())
	if err != nil {
		t.Fatalf("GetOrCreateCommit failed: %v", err)
	}

	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Agent: agent})
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}

	return job
}

// MakeJSONRequest creates an HTTP request with the given body marshaled as JSON.
func MakeJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()

	reqBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Failed to marshal request body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// ReceiveWithTimeout reads a single value from a channel, failing the test
// if nothing is received within the given timeout.
func ReceiveWithTimeout[T any](t *testing.T, ch <-chan T, timeout time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatal("Timed out waiting to receive from channel")
		var zero T
		return zero
	}
}

// WaitForJobStatus polls until the job reaches one of the expected statuses or
// the timeout expires. Returns the final job state.
func WaitForJobStatus(t *testing.T, db *storage.DB, jobID int64, timeout time.Duration, statuses ...storage.JobStatus) *storage.ReviewJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := db.GetJobByID(jobID)
		if err != nil {
			t.Fatalf("GetJobByID failed: %v", err)
		}
		if slices.Contains(statuses, job.Status) {
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Job %d did not reach any of %v within %v", jobID, statuses, timeout)
	return nil
}

// CreateCompletedReview creates a commit (if needed) and a completed review job.
// Returns the created job.
// NOTE: Uses ClaimJob which claims the next available job globally. This is safe
// when each test uses an isolated DB (via OpenTestDB), but callers must not
// enqueue multiple jobs before calling this helper.
func CreateCompletedReview(t *testing.T, db *storage.DB, repoID int64, sha, agent, reviewText string) *storage.ReviewJob {
	t.Helper()

	commit, err := db.GetOrCreateCommit(repoID, sha, GitUserName, "test commit", time.Now())
	if err != nil {
		t.Fatalf("Failed to create commit: %v", err)
	}

	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repoID, CommitID: commit.ID, GitRef: sha, Agent: agent})
	if err != nil {
		t.Fatalf("EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("test-worker"); err != nil {
		t.Fatalf("ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(job.ID, "test-worker", "prompt", reviewText); err != nil {
		t.Fatalf("CompleteJob failed: %v", err)
	}

	return job
}

// ReviewComment represents a comment on a review with a defined order.
type ReviewComment struct {
	User string
	Text string
}

// CreateReviewWithComments creates a completed review and adds comments to it.
// Comments are added in slice order to ensure deterministic behavior.
func CreateReviewWithComments(t *testing.T, db *storage.DB, repoID int64, sha, reviewText string, comments []ReviewComment) *storage.ReviewJob {
	t.Helper()

	job := CreateCompletedReview(t, db, repoID, sha, "test", reviewText)
	for _, c := range comments {
		if _, err := db.AddCommentToJob(job.ID, c.User, c.Text); err != nil {
			t.Fatalf("AddCommentToJob failed: %v", err)
		}
	}

	return job
}

// DecodeJSON unmarshals the response body from an httptest.ResponseRecorder into v.
func DecodeJSON(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()

	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}
}

// Ptr returns a pointer to the given value.
//
//go:fix inline
func Ptr[T any](v T) *T {
	return new(v)
}

// MapKeys returns the keys of the map m.
func MapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
