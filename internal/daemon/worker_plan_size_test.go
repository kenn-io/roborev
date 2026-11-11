package daemon

import (
	"context"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestWorkerPlanUsesCallerPromptTransport(t *testing.T) {
	for _, phase := range []string{"planning", "implementation"} {
		t.Run(phase, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			// Untracked local policy controls transport in both disposable worktrees.
			require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("max_prompt_size = 1000\n"), 0o600))
			planning := "Analyze cancellation"
			plan := "Check cancellation"
			if phase == "planning" {
				planning += strings.Repeat("context", 300)
			} else {
				plan += strings.Repeat("step", 600)
			}
			calls := 0
			var snapshotPaths []string
			name := "worker-plan-limit-" + phase
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, ref, p string, _ io.Writer) (string, error) {
				calls++
				text := p
				if rest, ok := strings.CutPrefix(p, "Read the complete task prompt from "); ok {
					quoted, _, _ := strings.Cut(rest, " and carry out")
					file, err := strconv.Unquote(quoted)
					require.NoError(t, err)
					rel, err := filepath.Rel(path, file)
					require.NoError(t, err)
					assert.True(t, filepath.IsLocal(rel))
					snapshotPaths = append(snapshotPaths, file)
					body, err := os.ReadFile(file)
					require.NoError(t, err)
					text = string(body)
				}
				if calls == 1 {
					assert.Equal(t, planning, text)
					return plan, nil
				}
				assert.Contains(t, text, "## Plan\n\n"+plan)
				require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
				return "Fixed", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			job := enqueuePlanTestJob(t, tc, name, prompt.EncodeFixPlan(planning, "## Review Findings\nHigh: missing cancellation"))
			tc.Pool.processJob(testWorkerID, job)
			assert.Equal(t, 2, calls)
			require.Len(t, snapshotPaths, 1)
			_, statErr := os.Stat(snapshotPaths[0])
			require.ErrorIs(t, statErr, os.ErrNotExist)
			updated, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			require.NotNil(t, updated.Patch)
			assert.Contains(t, *updated.Patch, "fixed.txt")
		})
	}
}

func TestWorkerReviewReceivesCurrentKataEvidence(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("max_prompt_size = 1000\n"), 0o600))
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata.Enabled = true
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	prior := tc.createAndClaimJob(t, sha, "prior-worker")
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, prior.ID, "test", "prompt", "High: original finding"))
	evidence := strings.Repeat("conflicting current evidence ", 400)
	reads := 0
	tc.Pool.readKataContext = func(_ context.Context, review *storage.Review) (any, error) {
		reads++
		assert.Equal(t, prior.ID, review.JobID)
		return map[string]any{"author": "reviewer", "body": evidence}, nil
	}
	name := "worker-kata-context"
	calls := 0
	var snapshot string
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		calls++
		if rest, ok := strings.CutPrefix(p, "Read the complete task prompt from "); ok {
			quoted, _, _ := strings.Cut(rest, " and carry out")
			var err error
			snapshot, err = strconv.Unquote(quoted)
			require.NoError(t, err)
			data, err := os.ReadFile(snapshot)
			require.NoError(t, err)
			p = string(data)
		}
		assert.Contains(t, p, "High: original finding")
		assert.Contains(t, p, evidence)
		assert.Contains(t, p, "untrusted-kata-discussion")
		return "No issues found.", nil
	}})
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, name)
	tc.Pool.processJob(testWorkerID, job)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, reads)
	if snapshot != "" {
		_, err := os.Stat(snapshot)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestWorkerRangeAgentReadsCurrentKataInPriorRangeFile(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	base := tc.GitRepo.HeadSHA()
	tc.GitRepo.CommitFile("first.go", "package first\n", "first")
	second := tc.GitRepo.CommitFile("second.go", "package second\n", "second")
	third := tc.GitRepo.CommitFile("third.go", "package third\n", "third")
	prior, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: base + ".." + second, Agent: "test", JobType: storage.JobTypeRange})
	require.NoError(t, err)
	_, err = tc.DB.ClaimJob("prior-worker")
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, prior.ID, "test", "prompt", "High: earlier finding"))
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata.Enabled = true
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	evidence := strings.Repeat("range reply evidence ", 300)
	reads := 0
	tc.Pool.readKataContext = func(_ context.Context, review *storage.Review) (any, error) {
		reads++
		assert.Equal(t, prior.ID, review.JobID)
		return map[string]any{"author": "reviewer", "body": evidence}, nil
	}
	name := "worker-range-kata-context"
	calls := 0
	var priorFile string
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		calls++
		if rest, ok := strings.CutPrefix(p, "Read the complete task prompt from "); ok {
			quoted, _, _ := strings.Cut(rest, " and carry out")
			file, err := strconv.Unquote(quoted)
			require.NoError(t, err)
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			p = string(data)
		}
		start := strings.Index(p, "<prior-range-reviews ")
		require.NotEqual(t, -1, start)
		var ref struct {
			File string `xml:"file,attr"`
		}
		require.NoError(t, xml.NewDecoder(strings.NewReader(p[start:])).Decode(&ref))
		priorFile = ref.File
		data, err := os.ReadFile(priorFile)
		require.NoError(t, err)
		assert.Contains(t, string(data), "High: earlier finding")
		assert.Contains(t, string(data), evidence)
		assert.Contains(t, string(data), "reviewer")
		return "No issues found.", nil
	}})
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: base + ".." + third, Agent: name, JobType: storage.JobTypeRange})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	tc.Pool.processJob(testWorkerID, claimed)
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, reads)
	require.NotEmpty(t, priorFile)
	_, err = os.Stat(priorFile)
	require.ErrorIs(t, err, os.ErrNotExist)
}
