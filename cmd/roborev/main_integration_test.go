//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

func TestRunRefineAgentErrorRetriesWithoutApplyingChanges(t *testing.T) {
	repoDir, headSHA := setupRefineRepo(t)

	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	md.State.reviews[headSHA] = &storage.Review{
		ID: 1, JobID: 7, Output: "**Bug found**: fail", Closed: false,
	}

	// Use 2 iterations so we can verify retry behavior
	agent.Register(&functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		return "", fmt.Errorf("test agent failure")
	}})
	defer agent.Register(agent.NewTestAgent())

	// Capture HEAD before running refine
	headBefore := gitRevParse(t, repoDir, "HEAD")

	ctx := defaultTestRunContext(t, repoDir)

	output := captureStdout(t, func() {
		// With 2 iterations and a failing agent, should exhaust iterations
		err := runRefine(ctx, refineOptions{agentName: "test", maxIterations: 2, quiet: true})
		require.Error(t, err)
	})

	// Verify agent error message is printed (not shadowed by ResolveSHA)
	assert.Contains(t, output, "Agent error: test agent failure")

	// Verify "Will retry in next iteration" message
	assert.Contains(t, output, "Will retry in next iteration")

	// Verify no commit was created (HEAD unchanged)
	headAfter := gitRevParse(t, repoDir, "HEAD")
	assert.Equal(t, headBefore, headAfter)

	// Verify we attempted 2 iterations (both printed)
	assert.Contains(t, output, "=== Refinement iteration 1/2 ===")
	assert.Contains(t, output, "=== Refinement iteration 2/2 ===")
}

func handleMockRefineGetJobs(t *testing.T) func(w http.ResponseWriter, r *http.Request, s *mockRefineState) bool {
	return func(w http.ResponseWriter, r *http.Request, s *mockRefineState) bool {
		q := r.URL.Query()
		if idStr := q.Get("id"); idStr != "" {
			var jobID int64
			fmt.Sscanf(idStr, "%d", &jobID)
			s.mu.Lock()
			job, ok := s.jobs[jobID]
			if !ok {
				s.mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{}})
				return true
			}
			jobCopy := *job
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{jobCopy}})
			return true
		}
		if gitRef := q.Get("git_ref"); gitRef != "" {
			s.mu.Lock()
			var job *storage.ReviewJob
			for _, j := range s.jobs {
				if j.GitRef == gitRef {
					job = j
					break
				}
			}
			if job == nil {
				job = &storage.ReviewJob{
					ID:       s.nextJobID,
					GitRef:   gitRef,
					Agent:    "test",
					Status:   storage.JobStatusDone,
					RepoPath: q.Get("repo"),
				}
				s.jobs[job.ID] = job
				s.nextJobID++
			}
			if _, ok := s.reviews[gitRef]; !ok {
				s.reviews[gitRef] = &storage.Review{
					ID:     job.ID + 1000,
					JobID:  job.ID,
					Output: "**Bug**: fix failed",
				}
			}
			jobCopy := *job
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{jobCopy}})
			return true
		}
		return false // fall through to base handler
	}
}

// TestRunRefineBranchReviewUsesEmptyAgent verifies that when refine
// enqueues a branch review (after all per-commit reviews pass), it
// passes an empty agent name so the server resolves it via the "review"
// workflow config — not the refine/fix agent. This is a regression test
// for #552.
func TestRunRefineBranchReviewUsesEmptyAgent(t *testing.T) {
	repoDir, headSHA := setupRefineRepo(t)

	var capturedEnqueueAgent string
	var enqueueCount int

	md := NewMockDaemon(t, MockRefineHooks{
		OnEnqueue: func(
			w http.ResponseWriter, r *http.Request,
			state *mockRefineState,
		) bool {
			var req daemon.EnqueueRequest
			json.NewDecoder(r.Body).Decode(&req)

			capturedEnqueueAgent = req.Agent
			enqueueCount++

			state.mu.Lock()
			jobID := state.nextJobID
			job := &storage.ReviewJob{
				ID:     jobID,
				GitRef: req.GitRef,
				Agent:  req.Agent,
				Status: storage.JobStatusDone,
			}
			state.jobs[jobID] = job
			state.nextJobID++

			// Create a passing review for the branch review
			state.reviews[req.GitRef] = &storage.Review{
				ID:     jobID + 1000,
				JobID:  jobID,
				Output: "No issues found.",
			}
			state.mu.Unlock()

			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(job)
			return true
		},
		// Return only existing jobs without auto-creating —
		// handleMockRefineGetJobs auto-creates failing reviews
		// for any git_ref, which would interfere with the branch
		// review flow.
		OnGetJobs: func(
			w http.ResponseWriter, r *http.Request,
			s *mockRefineState,
		) bool {
			q := r.URL.Query()
			s.mu.Lock()
			defer s.mu.Unlock()

			if idStr := q.Get("id"); idStr != "" {
				var jobID int64
				fmt.Sscanf(idStr, "%d", &jobID)
				if job, ok := s.jobs[jobID]; ok {
					json.NewEncoder(w).Encode(map[string]any{
						"jobs": []storage.ReviewJob{*job},
					})
				} else {
					json.NewEncoder(w).Encode(map[string]any{
						"jobs": []storage.ReviewJob{},
					})
				}
				return true
			}

			gitRef := q.Get("git_ref")
			var jobs []storage.ReviewJob
			for _, j := range s.jobs {
				if gitRef == "" || j.GitRef == gitRef {
					jobs = append(jobs, *j)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"jobs": jobs,
			})
			return true
		},
	})
	defer md.Close()

	// Per-commit review passes — so refine reaches the branch review
	md.State.reviews[headSHA] = &storage.Review{
		ID: 1, JobID: 7, Output: "No issues found.",
	}

	ctx := defaultTestRunContext(t, repoDir)

	err := runRefine(ctx, refineOptions{
		agentName:     "test",
		maxIterations: 3,
		quiet:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, enqueueCount,
		"expected exactly one enqueue call for branch review")
	assert.Empty(t, capturedEnqueueAgent,
		"branch review should use empty agent (server-resolved), "+
			"not the refine agent")
}

func TestRefineLoopStaysOnFailedFixChain(t *testing.T) {
	repoDir, _ := setupRefineRepo(t)

	if err := os.WriteFile(filepath.Join(repoDir, "second.txt"), []byte("second"), 0o644); err != nil {
		require.NoError(t, err)
	}
	execGit(t, repoDir, "add", "second.txt")
	execGit(t, repoDir, "commit", "-m", "second commit")

	commitList := strings.Fields(execGit(t, repoDir, "rev-list", "--reverse", "main..HEAD"))
	assert.Equal(t, false, len(commitList) < 2)
	oldestCommit := commitList[0]
	newestCommit := commitList[1]

	md := NewMockDaemon(t, MockRefineHooks{
		OnGetJobs: handleMockRefineGetJobs(t),
	})
	defer md.Close()

	md.State.nextJobID = 100
	md.State.reviews[oldestCommit] = &storage.Review{
		ID: 1, JobID: 1, Output: "**Bug**: old failure", Closed: false,
	}
	md.State.reviews[newestCommit] = &storage.Review{
		ID: 2, JobID: 2, Output: "**Bug**: new failure", Closed: false,
	}

	var changeCount int
	agent.Register(&functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		changeCount++
		change := fmt.Sprintf("fix %d", changeCount)
		if err := os.WriteFile(filepath.Join(repoPath, "fix.txt"), []byte(change), 0o644); err != nil {
			return "", err
		}
		if output != nil {
			_, _ = output.Write([]byte(change))
		}
		return change, nil
	}})
	defer agent.Register(agent.NewTestAgent())

	ctx := defaultTestRunContext(t, repoDir)

	err := runRefine(ctx, refineOptions{agentName: "test", maxIterations: 2, quiet: true})
	require.Error(t, err)

	for _, call := range md.State.respondCalled {
		assert.NotEqual(t, int64(2), call.jobID, "expected to stay on failed fix chain; saw response for newer commit job 2")
	}
}
