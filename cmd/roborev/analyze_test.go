package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestExpandAndReadFiles(t *testing.T) {
	tmpDir := writeTestFiles(t, map[string]string{
		"main.go":            "package main\n",
		"util.go":            "package main\n// util\n",
		"sub/helper.go":      "package sub\n",
		"sub/helper_test.go": "package sub\n// test\n",
		"data.json":          `{"key": "value"}`,
		"README.md":          "# README\n",
		".hidden/secret.go":  "package hidden\n",
	})

	tests := []struct {
		name     string
		patterns []string
		wantKeys []string
		wantErr  bool
	}{
		{
			name:     "single file",
			patterns: []string{"main.go"},
			wantKeys: []string{"main.go"},
		},
		{
			name:     "glob pattern",
			patterns: []string{"*.go"},
			wantKeys: []string{"main.go", "util.go"},
		},
		{
			name:     "subdirectory file",
			patterns: []string{"sub/helper.go"},
			wantKeys: []string{"sub/helper.go"},
		},
		{
			name:     "subdirectory glob",
			patterns: []string{"sub/*.go"},
			wantKeys: []string{"sub/helper.go", "sub/helper_test.go"},
		},
		{
			name:     "multiple patterns",
			patterns: []string{"main.go", "sub/helper.go"},
			wantKeys: []string{"main.go", "sub/helper.go"},
		},
		{
			name:     "non-source file",
			patterns: []string{"data.json"},
			wantKeys: []string{"data.json"},
		},
		{
			name:     "directory as pattern",
			patterns: []string{"sub"},
			wantKeys: []string{"sub/helper.go", "sub/helper_test.go"},
		},
		{
			name:     "no match",
			patterns: []string{"nonexistent.go"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := expandAndReadFiles(tmpDir, tmpDir, tt.patterns)
			if tt.wantErr {
				require.Error(t, err, "expected error, got nil")
				return
			}
			require.NoError(t, err)

			assertFilesExist(t, files, tt.wantKeys)
		})
	}
}

func TestExpandAndReadFilesRecursive(t *testing.T) {
	tmpDir := writeTestFiles(t, map[string]string{
		"main.go":           "package main",
		"cmd/app/app.go":    "package app",
		"internal/util.go":  "package internal",
		"vendor/dep/dep.go": "package dep", // Should be skipped
		".git/config":       "[core]",      // Should be skipped
	})

	result, err := expandAndReadFiles(tmpDir, tmpDir, []string{"./..."})
	require.NoError(t, err, "expandAndReadFiles error")

	// Should include main.go, cmd/app/app.go, internal/util.go
	// Should NOT include vendor/dep/dep.go or .git/config
	assertFilesExist(t, result, []string{"main.go", "cmd/app/app.go", "internal/util.go"})

	noWant := []string{"vendor/dep/dep.go", ".git/config"}
	for _, nw := range noWant {
		nativeNW := filepath.FromSlash(nw)
		_, exists := result[nativeNW]
		assert.False(t, exists, "should not include %q", nativeNW)
	}
}

func TestExpandAndReadFiles_ShellExpanded(t *testing.T) {
	// Simulate shell-expanded globs: the shell expands *.go in a subdirectory
	// into individual relative file paths like "helper.go", "helper_test.go".
	// workDir is the subdirectory where the shell ran; repoRoot is the repo root.
	tmpDir := writeTestFiles(t, map[string]string{
		"main.go":            "package main\n",
		"sub/helper.go":      "package sub\n",
		"sub/helper_test.go": "package sub\n// test\n",
	})

	repoRoot := tmpDir
	workDir := filepath.Join(tmpDir, "sub")

	// Shell in sub/ expands *.go → ["helper.go", "helper_test.go"]
	files, err := expandAndReadFiles(workDir, repoRoot, []string{"helper.go", "helper_test.go"})
	require.NoError(t, err)

	// Keys should be relative to repoRoot
	assertFilesExist(t, files, []string{"sub/helper.go", "sub/helper_test.go"})
}

func TestExpandAndReadFiles_RecursiveFromSubdir(t *testing.T) {
	// ./... should walk from repoRoot and return all source files,
	// even when workDir is a subdirectory.
	tmpDir := writeTestFiles(t, map[string]string{
		"main.go":       "package main\n",
		"sub/helper.go": "package sub\n",
	})

	repoRoot := tmpDir
	workDir := filepath.Join(tmpDir, "sub")

	files, err := expandAndReadFiles(workDir, repoRoot, []string{"./..."})
	require.NoError(t, err)

	// Should find files across the entire repo, not just the subdirectory
	assertFilesExist(t, files, []string{"main.go", "sub/helper.go"})
}

func TestIsSourceFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"script.py", true},
		{"app.js", true},
		{"app.ts", true},
		{"app.tsx", true},
		{"lib.rs", true},
		{"config.yaml", true},
		{"config.yml", true},
		{"data.json", true},
		{"readme.md", true},
		{"binary.exe", false},
		{"image.png", false},
		{"archive.tar.gz", false},
		{".gitignore", false},
		{"Makefile", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isSourceFile(tt.path)
			assert.Equal(t, tt.want, got, "isSourceFile(%q)", tt.path)
		})
	}
}

func assertContains(t *testing.T, s, substr, msg string) {
	t.Helper()
	require.Contains(t, s, substr, "%s: expected string to contain %q\nDocument content:\n%s", msg, substr, s)
}

func TestBuildFixPrompt(t *testing.T) {
	analysisType := &analyze.AnalysisType{
		Name:        "refactor",
		Description: "Suggest refactoring opportunities",
	}
	analysisOutput := `## CODE SMELLS
- Long function in main.go:50
- Duplicated logic in utils.go

## REFACTORING SUGGESTIONS
- Extract method for repeated code`

	prompt := buildFixPrompt(analysisType, analysisOutput)

	assertContains(t, prompt, "refactor", "prompt should include analysis type name")
	assertContains(t, prompt, "CODE SMELLS", "prompt should include analysis output")
	assertContains(t, prompt, "Long function in main.go", "prompt should include specific findings")
	assertContains(t, prompt, "apply the suggested changes", "prompt should include fix instructions")
	assertContains(t, prompt, "compiles", "prompt should mention verification steps")
	assertContains(t, prompt, "tests", "prompt should mention verification steps")
}

func TestBuildFixPromptWithCommitMetadata(t *testing.T) {
	analysisType := &analyze.AnalysisType{
		Name:        "refactor",
		Description: "Suggest refactoring opportunities",
	}
	metadata := config.FixCommitMetadata{
		Author:    "Fix Author <fix-author@example.com>",
		CoAuthors: []string{"Reviewer One <one@example.com>"},
	}

	prompt := buildFixPromptWithMetadata(analysisType, "Found bug", metadata)

	assertContains(t, prompt, "## Commit Metadata", "prompt should include commit metadata section")
	assertContains(t, prompt, "Use this commit author: `Fix Author <fix-author@example.com>`", "prompt should include author instruction")
	assertContains(t, prompt, "Co-authored-by: Reviewer One <one@example.com>", "prompt should include co-author trailer")
}

func TestBuildFixPromptOmitsEmptyCommitMetadata(t *testing.T) {
	analysisType := &analyze.AnalysisType{
		Name:        "refactor",
		Description: "Suggest refactoring opportunities",
	}

	prompt := buildFixPromptWithMetadata(analysisType, "Found bug", config.FixCommitMetadata{})

	assert.NotContains(t, prompt, "## Commit Metadata")
}

func TestAnalyzeOptionsDefaults(t *testing.T) {
	opts := analyzeOptions{
		agentName: "gemini",
		fix:       true,
	}

	// fixAgent should default to agentName when empty
	fixAgent := opts.fixAgent
	if fixAgent == "" {
		fixAgent = opts.agentName
	}
	require.Equal(t, "gemini", fixAgent, "fixAgent should default to agentName")

	// When fixAgent is set, it should be used
	opts.fixAgent = "claude-code"
	require.Equal(t, "claude-code", opts.fixAgent, "fixAgent should be 'claude-code'")
}

func TestWaitForAnalysisJob_Timeout(t *testing.T) {
	// Test that context timeout is respected
	// JobIDStart: 43 means the initial job ID (without enqueue) is 42.
	// DoneAfterPolls: 999 ensures it stays "queued" long enough for timeout.
	ts, _ := newMockServer(t, MockServerOpts{
		JobIDStart:     43,
		DoneAfterPolls: 999,
	})
	// patchServerAddr not strictly needed as we pass ts.URL to waitForAnalysisJob,
	// but good practice if waitForAnalysisJob used the global.
	// Here waitForAnalysisJob takes the url arg.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := waitForAnalysisJob(ctx, mustParseEndpoint(t, ts.URL), 42)
	require.Error(t, err, "expected timeout error")
	require.ErrorContains(t, err, "context deadline exceeded", "expected context deadline error")
}

func mockCloseServer(t *testing.T, expectedID int64, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/review/close", r.URL.Path, "unexpected path")
		assert.Equal(t, http.MethodPost, r.Method, "unexpected method")

		var req map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req), "failed to decode body")
		gotJobID := int64(req["job_id"].(float64))
		gotClosed := req["closed"].(bool)

		assert.Equal(t, expectedID, gotJobID, "job_id")
		assert.True(t, gotClosed, "closed should be true")

		w.WriteHeader(statusCode)
	}))
}

func TestMarkJobClosed(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"success", http.StatusOK, false},
		{"server error", http.StatusInternalServerError, true},
		{"not found", http.StatusNotFound, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := mockCloseServer(t, 123, tt.statusCode)
			defer ts.Close()

			err := markJobClosed(context.Background(), ts.URL, 123)

			if tt.wantErr {
				require.Error(t, err, "expected error, got nil")
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestRunFixAgent(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755); err != nil {
		require.NoError(t, err, "MkdirAll: %v")
	}

	cmd, output := newTestCmd(t)

	// Use the built-in test agent
	err := runFixAgent(cmd, tmpDir, "test", "", "fast", "Fix the issues", false)
	require.NoError(t, err, "runFixAgent failed: %v")

	// Test agent should produce output
	require.NotZero(t, output.Len(), "expected output from test agent")
}

func TestBuildCommitPrompt(t *testing.T) {
	analysisType := &analyze.AnalysisType{
		Name:        "duplication",
		Description: "Find code duplication",
	}

	prompt := buildCommitPrompt(analysisType)

	// Should mention the analysis type
	assert.Contains(t, prompt, "duplication", "prompt should reference the analysis type")

	// Should have instructions for committing
	assert.Contains(t, prompt, "git commit", "prompt should mention git commit")

	// Should ask for a descriptive message
	assert.Contains(t, prompt, "descriptive", "prompt should request a descriptive message")
}

func TestBuildCommitPromptWithCommitMetadata(t *testing.T) {
	analysisType := &analyze.AnalysisType{
		Name:        "duplication",
		Description: "Find code duplication",
	}
	metadata := config.FixCommitMetadata{
		Author:    "Fix Author <fix-author@example.com>",
		CoAuthors: []string{"Reviewer One <one@example.com>"},
	}

	prompt := buildCommitPromptWithMetadata(analysisType, metadata)

	assert.Contains(t, prompt, "## Commit Metadata")
	assert.Contains(t, prompt, "Use this commit author: `Fix Author <fix-author@example.com>`")
	assert.Contains(t, prompt, "Co-authored-by: Reviewer One <one@example.com>")
}

func TestListAnalysisTypes(t *testing.T) {
	cmd, output := newTestCmd(t)

	err := listAnalysisTypes(cmd)
	require.NoError(t, err, "listAnalysisTypes failed: %v")

	outputStr := output.String()

	assertContains(t, outputStr, "Available analysis types", "output should contain header")

	// Should list all types
	expectedTypes := []string{
		"test-fixtures",
		"duplication",
		"refactor",
		"complexity",
		"api-design",
		"dead-code",
		"architecture",
		"security",
	}

	for _, typ := range expectedTypes {
		assertContains(t, outputStr, typ, "output should contain analysis type")
	}
}

func TestShowAnalysisPrompt(t *testing.T) {
	cmd, output := newTestCmd(t)

	err := showAnalysisPrompt(cmd, "test-fixtures")
	require.NoError(t, err, "showAnalysisPrompt failed")

	outputStr := output.String()

	assert.Contains(t, outputStr, "# test-fixtures", "output should contain type name header")
	assert.Contains(t, outputStr, "Description:", "output should contain description")
	assert.Contains(t, outputStr, "## Prompt Template", "output should contain prompt template section")

	// Verify there's substantial content after the template header
	// (the prompt templates are all multi-line with instructions)
	_, after, ok := strings.Cut(outputStr, "## Prompt Template")
	assert.True(t, ok, "should have prompt template section")
	if ok {
		afterHeader := after
		assert.GreaterOrEqual(t, len(strings.TrimSpace(afterHeader)), 50, "prompt template section should have substantial content")
	}
}

func TestShowAnalysisPromptUnknown(t *testing.T) {
	cmd, _ := newTestCmd(t)

	err := showAnalysisPrompt(cmd, "unknown-type")
	require.Error(t, err, "expected error for unknown type")
	assert.Contains(t, err.Error(), "unknown analysis type", "error should mention 'unknown analysis type'")
}

func TestShowPromptRequiresType(t *testing.T) {
	cmd := analyzeCmd()
	cmd.SetArgs([]string{"--show-prompt"})
	err := cmd.Execute()
	require.Error(t, err, "expected error when --show-prompt used without type")
	assert.Contains(t, err.Error(), "requires an analysis type", "error should mention 'requires an analysis type'")
}

func TestPerFileAnalysis(t *testing.T) {
	ts, state := newMockServer(t, MockServerOpts{})

	tmpDir := writeTestFiles(t, map[string]string{
		"a.go": "package a\n",
		"b.go": "package b\n",
		"c.go": "package c\n",
	})

	files, err := expandAndReadFiles(tmpDir, tmpDir, []string{"*.go"})
	require.NoError(t, err, "expandAndReadFiles")

	cmd, output := newTestCmd(t)

	analysisType := analyze.GetType("refactor")
	err = runPerFileAnalysis(t.Context(), cmd, mustParseEndpoint(t, ts.URL), tmpDir, analysisType, files, analyzeOptions{quiet: false}, config.DefaultMaxPromptSize)
	require.NoError(t, err, "runPerFileAnalysis")

	// Should have created 3 jobs (one per file)
	assert.Equal(t, int32(3), atomic.LoadInt32(&state.EnqueueCount), "expected 3 jobs")

	// Output should mention all job IDs
	outputStr := output.String()
	assert.Contains(t, outputStr, "Created 3 jobs", "output should mention 3 jobs created")
}

func TestEnqueueAnalysisJob(t *testing.T) {
	ts, _ := newMockServer(t, MockServerOpts{
		JobIDStart: 42,
		OnEnqueue: func(w http.ResponseWriter, r *http.Request) {
			var req daemon.EnqueueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			assert.True(t, req.Agentic, "agentic should be true for analysis")
			assert.NotEmpty(t, req.CustomPrompt, "custom_prompt should be set")

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(storage.ReviewJob{
				ID:     42,
				Agent:  "test",
				Status: storage.JobStatusQueued,
			})
		},
	})

	job, err := enqueueAnalysisJob(t.Context(), mustParseEndpoint(t, ts.URL), "/repo", "test prompt", "", "test-fixtures", analyzeOptions{agentName: "test"})
	require.NoError(t, err, "enqueueAnalysisJob")

	assert.Equal(t, int64(42), job.ID, "job.ID")
}

func TestEnqueueSecurityAnalysisJobUsesSecurityReviewType(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("main.go", "package main", "initial")

	var gotReviewType string
	ts, _ := newMockServer(t, MockServerOpts{
		OnEnqueue: func(w http.ResponseWriter, r *http.Request) {
			var req daemon.EnqueueRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotReviewType = req.ReviewType

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(storage.ReviewJob{
				ID:     42,
				Agent:  "test",
				Status: storage.JobStatusQueued,
			})
		},
	})

	_, err := enqueueAnalysisJob(t.Context(), mustParseEndpoint(t, ts.URL), repo.Dir, "test prompt", "", "security", analyzeOptions{agentName: "test"})
	require.NoError(t, err, "enqueueAnalysisJob")

	assert.Equal(t, "security", gotReviewType, "security analysis should resolve security workflow config")
}

func TestEnqueueAnalysisJobAppliesAnalyzeConfig(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("main.go", "package main", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, ".roborev.toml"), []byte(`
[analyze.refactor]
agent = "configured-agent"
model = "configured-model"
reasoning = "fast"
`), 0o644))

	var got daemon.EnqueueRequest
	ts, _ := newMockServer(t, MockServerOpts{
		OnEnqueue: func(w http.ResponseWriter, r *http.Request) {
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(storage.ReviewJob{
				ID:     42,
				Agent:  "configured-agent",
				Status: storage.JobStatusQueued,
			})
		},
	})

	_, err := enqueueAnalysisJob(t.Context(), mustParseEndpoint(t, ts.URL), repo.Dir, "test prompt", "", "refactor", analyzeOptions{})
	require.NoError(t, err, "enqueueAnalysisJob")

	assert.Equal(t, "configured-agent", got.Agent)
	assert.Equal(t, "configured-model", got.Model)
	assert.Equal(t, "fast", got.Reasoning)
	assert.Empty(t, got.ReviewType)
}

func TestEnqueueAnalysisJobBranchName(t *testing.T) {
	// Create a real git repo so GetCurrentBranch returns a known value
	repo := newTestGitRepo(t)
	repo.Run("checkout", "-b", "test-current")
	repo.CommitFile("init.go", "package main", "initial")

	captureBranch := func(t *testing.T) (mock *httptest.Server, gotBranch *string) {
		t.Helper()
		var branch string
		ts, _ := newMockServer(t, MockServerOpts{
			OnEnqueue: func(w http.ResponseWriter, r *http.Request) {
				var req daemon.EnqueueRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				branch = req.Branch
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(storage.ReviewJob{ID: 1, Agent: "test", Status: storage.JobStatusQueued})
			},
		})
		return ts, &branch
	}

	t.Run("no branch flag uses current branch", func(t *testing.T) {
		ts, gotBranch := captureBranch(t)

		_, err := enqueueAnalysisJob(t.Context(), mustParseEndpoint(t, ts.URL), repo.Dir, "prompt", "", "refactor", analyzeOptions{})
		require.NoError(t, err, "enqueueAnalysisJob")
		assert.Equal(t, "test-current", *gotBranch, "expected branch 'test-current'")
	})

	t.Run("branch=HEAD uses current branch", func(t *testing.T) {
		ts, gotBranch := captureBranch(t)

		_, err := enqueueAnalysisJob(t.Context(), mustParseEndpoint(t, ts.URL), repo.Dir, "prompt", "", "refactor", analyzeOptions{branch: "HEAD"})
		require.NoError(t, err, "enqueueAnalysisJob")
		assert.Equal(t, "test-current", *gotBranch, "expected branch 'test-current'")
	})

	t.Run("named branch overrides current branch", func(t *testing.T) {
		ts, gotBranch := captureBranch(t)

		_, err := enqueueAnalysisJob(t.Context(), mustParseEndpoint(t, ts.URL), repo.Dir, "prompt", "", "refactor", analyzeOptions{branch: "feature-xyz"})
		require.NoError(t, err, "enqueueAnalysisJob")
		assert.Equal(t, "feature-xyz", *gotBranch, "expected branch 'feature-xyz'")
	})
}

func setupBranchTestRepo(t *testing.T) *TestGitRepo {
	repo := newTestGitRepo(t)
	repo.Run("checkout", "-b", "main")
	repo.CommitFile("base.go", "package main", "base")

	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("new.go", "package main\nfunc New() {}", "add go file")
	repo.CommitFile("docs.md", "# Docs", "add docs")
	repo.CommitFile("config.yml", "key: val", "add config")
	return repo
}

func TestGetBranchFiles(t *testing.T) {
	cmd, _ := newTestCmd(t)

	t.Run("filters to code files only", func(t *testing.T) {
		repo := setupBranchTestRepo(t)
		files, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{
			branch:     "HEAD",
			baseBranch: "main",
		})
		require.NoError(t, err, "getBranchFiles")
		assert.Len(t, files, 1, "expected 1 code file")
		assert.Contains(t, files, "new.go", "expected new.go in results")
	})

	t.Run("reads from git not working tree", func(t *testing.T) {
		repo := setupBranchTestRepo(t)
		// Modify working tree — should NOT affect branch analysis
		_ = os.WriteFile(filepath.Join(repo.Dir, "new.go"), []byte("DIRTY"), 0o644)

		files, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{
			branch:     "HEAD",
			baseBranch: "main",
		})
		require.NoError(t, err, "getBranchFiles")
		content := files["new.go"]
		assert.NotContains(t, content, "DIRTY", "getBranchFiles should read from git HEAD, not working tree")
		assert.Contains(t, content, "func New()", "expected committed content")
	})

	t.Run("named branch reads from that branch", func(t *testing.T) {
		repo := setupBranchTestRepo(t)
		// Switch back to main, analyze feature branch by name
		repo.Run("checkout", "main")

		files, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{
			branch:     "feature",
			baseBranch: "main",
		})
		require.NoError(t, err, "getBranchFiles")
		assert.Contains(t, files, "new.go", "expected new.go from feature branch")
	})

	t.Run("no code files returns error", func(t *testing.T) {
		repo := setupBranchTestRepo(t)
		repo.Run("checkout", "-b", "docs-only", "main")
		repo.CommitFile("readme.md", "# Hello", "add readme")

		_, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{
			branch:     "HEAD",
			baseBranch: "main",
		})
		require.Error(t, err, "expected error for no code files")
		assert.Contains(t, err.Error(), "no code files changed", "unexpected error")
	})

	t.Run("on base branch returns error", func(t *testing.T) {
		repo := setupBranchTestRepo(t)
		repo.Run("checkout", "main")

		_, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{
			branch:     "HEAD",
			baseBranch: "main",
		})
		require.Error(t, err, "expected error when on base branch")
		assert.Contains(t, err.Error(), "already on main", "unexpected error")
	})

	t.Run("prefers non-origin upstream as base", func(t *testing.T) {
		// Fork workflow: origin (stale fork) and upstream (real repo).
		// Feature branch tracks upstream/main; merge-base must be taken
		// against upstream/main, not origin/main, so commits already
		// merged upstream aren't re-analyzed.
		remote := newBareTestGitRepo(t)
		stale := newBareTestGitRepo(t)
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("a.go", "package main", "u1")
		repo.CommitFile("b.go", "package main", "u2")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")

		// origin lags upstream by 1 commit.
		repo.Run("remote", "add", "origin", stale.Dir)
		repo.Run("push", "origin", "HEAD~1:refs/heads/main")
		repo.Run("fetch", "origin")
		repo.Run("remote", "set-head", "origin", "main")

		repo.Run("checkout", "-b", "feature", "--track", "upstream/main")
		repo.CommitFile("only-new.go", "package main\nfunc New() {}", "feature work")

		files, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{branch: "HEAD"})
		require.NoError(t, err)
		// b.go was already merged to upstream/main; must not be analyzed.
		assert.NotContains(t, files, "b.go",
			"already-merged upstream commit must not be analyzed")
		assert.Contains(t, files, "only-new.go",
			"new feature file should be analyzed")
	})

	t.Run("blocks local main tracking non-origin upstream", func(t *testing.T) {
		remote := newBareTestGitRepo(t)
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("a.go", "package main", "initial")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")

		_, err := getBranchFiles(t.Context(), cmd, repo.Dir, analyzeOptions{branch: "HEAD"})
		require.Error(t, err, "expected error when on base branch")
		assert.Contains(t, err.Error(), "already on main", "unexpected error")
	})
}

func TestAnalyzeBranchSpaceSeparated(t *testing.T) {
	// Verify that `--branch feature-xyz` (space-separated) gives a helpful error
	// because NoOptDefVal causes "feature-xyz" to become a positional arg
	cmd := analyzeCmd()
	cmd.SetArgs([]string{"refactor", "--branch", "feature-xyz"})
	err := cmd.Execute()
	require.Error(t, err, "expected error")
	assert.Contains(t, err.Error(), "--branch=<name>", "error should suggest --branch=<name> syntax")
}

func TestAnalyzeBranchFlagValidation(t *testing.T) {
	t.Run("branch requires analysis type", func(t *testing.T) {
		cmd := analyzeCmd()
		cmd.SetArgs([]string{"--branch=feature"})

		err := cmd.Execute()
		require.ErrorContains(t, err, "--branch requires an analysis type")
	})

	t.Run("branch cannot be combined with file patterns", func(t *testing.T) {
		cmd := analyzeCmd()
		cmd.SetArgs([]string{"refactor", "--branch=feature", "*.go"})

		err := cmd.Execute()
		require.ErrorContains(t, err, "cannot specify file patterns with --branch")
	})
}

func TestAnalyzeJSONOutput(t *testing.T) {
	t.Run("single analysis JSON output", func(t *testing.T) {
		ts, _ := newMockServer(t, MockServerOpts{Agent: "test-agent"})

		tmpDir := writeTestFiles(t, map[string]string{"test.go": "package main"})

		files := map[string]string{"test.go": "package main"}
		analysisType := analyze.GetType("refactor")

		cmd, output := newTestCmd(t)

		err := runSingleAnalysis(t.Context(), cmd, mustParseEndpoint(t, ts.URL), tmpDir, analysisType, files, analyzeOptions{jsonOutput: true}, config.DefaultMaxPromptSize)
		require.NoError(t, err, "runSingleAnalysis: %v")

		var result AnalyzeResult
		require.NoError(t, json.Unmarshal(output.Bytes(), &result), "failed to parse JSON output")

		assert.Len(t, result.Jobs, 1, "expected 1 job")
		assert.Equal(t, int64(1), result.Jobs[0].ID, "job ID")
		assert.Equal(t, "test-agent", result.Jobs[0].Agent, "agent")
		assert.Equal(t, "refactor", result.AnalysisType, "analysis type")
		assert.Equal(t, []string{"test.go"}, result.Files, "files")
	})

	t.Run("per-file analysis JSON output", func(t *testing.T) {
		ts, _ := newMockServer(t, MockServerOpts{Agent: "test-agent"})

		files := map[string]string{
			"a.go": "package a",
			"b.go": "package b",
			"c.go": "package c",
		}
		tmpDir := writeTestFiles(t, files)

		analysisType := analyze.GetType("complexity")

		cmd, output := newTestCmd(t)

		err := runPerFileAnalysis(t.Context(), cmd, mustParseEndpoint(t, ts.URL), tmpDir, analysisType, files, analyzeOptions{jsonOutput: true}, config.DefaultMaxPromptSize)
		require.NoError(t, err, "runPerFileAnalysis")

		var result AnalyzeResult
		require.NoError(t, json.Unmarshal(output.Bytes(), &result), "failed to parse JSON output")

		assert.Len(t, result.Jobs, 3, "expected 3 jobs")
		// Jobs should be in sorted file order
		expectedFiles := []string{"a.go", "b.go", "c.go"}
		for i, info := range result.Jobs {
			assert.Equal(t, expectedFiles[i], info.File, "job %d: expected file", i)
			assert.Equal(t, int64(i+1), info.ID, "job %d: expected ID", i)
		}
		assert.Equal(t, "complexity", result.AnalysisType, "expected analysis type 'complexity'")
		assert.Len(t, result.Files, 3, "expected 3 files")
	})
}

func assertFilesExist(t *testing.T, files map[string]string, wantKeys []string) {
	t.Helper()
	require.Len(t, files, len(wantKeys), "got %d files, want %d: %v", len(files), len(wantKeys), testutil.MapKeys(files))
	for _, key := range wantKeys {
		nativeKey := filepath.FromSlash(key)
		require.Contains(t, files, nativeKey, "missing expected file %q, got keys: %v", nativeKey, testutil.MapKeys(files))
	}
}
