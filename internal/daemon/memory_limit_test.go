//go:build !windows

package daemon

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/proctrack"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// runMemoryHog starts a tracked command whose dd fills a 300 MiB buffer, then
// blocks writing it to a reader that never reads, holding the memory.
func runMemoryHog(ctx context.Context) error {
	return proctrack.Run(ctx, exec.CommandContext(ctx, "/bin/sh", "-c",
		"dd if=/dev/zero bs=314572800 count=1 2>/dev/null | sleep 120"))
}

func TestWorkerMemoryBreachFailsJobWithoutBackup(t *testing.T) {
	const hog = "memory-hog-test"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: hog,
		ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			return "", fmt.Errorf("agent exited: %w", runMemoryHog(ctx))
		},
	})
	tc := newWorkerTestContext(t, 2)
	cfg := config.DefaultConfig()
	cfg.AgentMemoryMaxMB = 64
	cfg.DefaultBackupAgent = "test"
	tc.reconfigurePool(cfg)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, hog)

	tc.Pool.processJob(testWorkerID, job)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, failed.Error, "agent processes' summed RSS reached")
	assert.Contains(t, failed.Error, "over agent_memory_max_mb (64 MiB)")
	assert.Equal(t, hog, failed.Agent, "a breach must not fail over to the backup agent")
	assert.Equal(t, 0, failed.RetryCount)

	// The breach belongs to that attempt alone: the next job runs normally.
	next := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "test")
	tc.Pool.processJob(testWorkerID, next)
	tc.assertJobStatus(t, next.ID, storage.JobStatusDone)
}

func TestCICheckMemoryBreachKeepsReview(t *testing.T) {
	tc, job, doc, original := setupCICheckCompletion(t, "high", "", nil)
	tc.Pool.ciCheck = func(ctx context.Context, _ *storage.ReviewJob, _ ciCheckConfig, _ structuredreview.Document, _ []string, _ io.Writer) (structuredreview.Document, error) {
		return structuredreview.Document{}, fmt.Errorf("run CI checker: %w", runMemoryHog(ctx))
	}
	ctx, scope := proctrack.New(t.Context(), 64<<20)
	defer scope.Close()

	tc.Pool.processSynthesisJob(ctx, testWorkerID, job)

	require.NoError(t, ctx.Err(), "a checker breach must not cancel the attempt")
	finished, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, finished.Status)
	stored, checked := readCICheckCompletionReview(t, tc, job)
	assert.Equal(t, original.review.Output, stored.Output)
	assert.Equal(t, original.review.Verdict, stored.Verdict())
	doc.Check = &structuredreview.Check{Status: structuredreview.CheckFailed}
	assert.Equal(t, doc, checked)
	log, err := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, err)
	assert.Contains(t, string(log), "CI checker failed; keeping original review: agent processes' summed RSS reached")
}

// The classifier tries its backup on any primary error, so a breach must stop
// it there.
func TestClassifyMemoryBreachSkipsBackup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	backupInvoked := make(chan struct{}, 1)
	primaryName := "memory-classifier-primary"
	backupName := "memory-classifier-backup"
	agent.RegisterForTest(t, &fakeSchemaAgent{
		name: primaryName,
		classifyFn: func(ctx context.Context) (jsontext.Value, error) {
			return nil, runMemoryHog(ctx)
		},
	})
	agent.RegisterForTest(t, &fakeSchemaAgent{
		name: backupName,
		classifyFn: func(context.Context) (jsontext.Value, error) {
			backupInvoked <- struct{}{}
			return []byte(`{"design_review":false,"reason":"backup"}`), nil
		},
	})
	cfg := config.DefaultConfig()
	cfg.AgentMemoryMaxMB = 64
	cfg.ClassifyAgent = primaryName
	cfg.ClassifyBackupAgent = backupName
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	jobID, err := tc.DB.EnqueueAutoDesignJob(storage.EnqueueOpts{
		RepoID:      tc.Repo.ID,
		GitRef:      "memory-classifier",
		Agent:       storage.AutoDesignAgentSentinel,
		JobType:     storage.JobTypeClassify,
		ReviewType:  "design",
		DiffContent: "+diff\n",
		Prompt:      "subject",
	})
	require.NoError(t, err)
	job, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, jobID, job.ID)

	tc.Pool.processJob(testWorkerID, job)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, failed.Error, "over agent_memory_max_mb (64 MiB)")
	assert.Empty(t, backupInvoked)
}

// A synthesis agent canceled by a breach used to look like a user
// cancellation, which records nothing, so the job stayed running.
func TestSynthesisMemoryBreachFailsJob(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.AgentMemoryMaxMB = 64
	tc.reconfigurePool(cfg)
	const synth = "memory-hog-synthesis"
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: synth, ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
		return "", fmt.Errorf("agent exited: %w", runMemoryHog(ctx))
	}})
	run, members, _ := enqueuePanelRun(t, tc, "memory-panel", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
	setSynthesisAgent(t, tc, run, synth)
	completeMember(t, tc, members[0].ID, "test", "first finding")
	completeMember(t, tc, members[1].ID, "test", "second finding")
	job := releaseAndClaimSynthesis(t, tc, run)

	tc.Pool.processJob(testWorkerID, job)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, failed.Error, "over agent_memory_max_mb (64 MiB)")
}

// A checker that returns verdicts after its processes breached must not have
// them applied: the breach decides the check's outcome.
func TestCICheckBreachOverridesReturnedVerdicts(t *testing.T) {
	tc, job, doc, original := setupCICheckCompletion(t, "high", "", nil)
	tc.Pool.ciCheck = func(ctx context.Context, _ *storage.ReviewJob, _ ciCheckConfig, doc structuredreview.Document, _ []string, _ io.Writer) (structuredreview.Document, error) {
		_ = runMemoryHog(ctx)
		doc.Findings = doc.Findings[:1]
		return doc, nil
	}
	ctx, scope := proctrack.New(t.Context(), 64<<20)
	defer scope.Close()

	tc.Pool.processSynthesisJob(ctx, testWorkerID, job)

	stored, checked := readCICheckCompletionReview(t, tc, job)
	assert.Equal(t, original.review.Output, stored.Output)
	doc.Check = &structuredreview.Check{Status: structuredreview.CheckFailed}
	assert.Equal(t, doc, checked)
}

// A synthesis agent that returns a document after its processes breached used
// to leave the job running: the check saw the canceled attempt and skipped
// completion, and no other path recorded an outcome.
func TestSynthesisBreachBeforeCheckFailsJob(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.AgentMemoryMaxMB = 64
	tc.reconfigurePool(cfg)
	const synth = "memory-hog-synthesis-result"
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: synth, ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
		_ = runMemoryHog(ctx)
		<-ctx.Done()
		return `{"schema_version":3,"summary":"combined","verdict":"fail","findings":[{"severity":"high","kind":"defect","confidence":0.9,"problem":"Build claim","fix":"Fix build","sources":[1]}]}`, nil
	}})
	tc.Pool.ciCheck = func(ctx context.Context, _ *storage.ReviewJob, _ ciCheckConfig, _ structuredreview.Document, _ []string, _ io.Writer) (structuredreview.Document, error) {
		return structuredreview.Document{}, ctx.Err()
	}
	run, members, _ := enqueuePanelRun(t, tc, "memory-check-panel", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
	setSynthesisAgent(t, tc, run, synth)
	encoded, err := (synthesisJobConfig{Check: &ciCheckConfig{Reasoning: "high"}}).encode()
	require.NoError(t, err)
	_, err = tc.DB.ExecContext(t.Context(), "UPDATE review_jobs SET panel_member_config_json = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'", encoded, run)
	require.NoError(t, err)
	completeMember(t, tc, members[0].ID, "test", "first finding")
	completeMember(t, tc, members[1].ID, "test", "second finding")
	job := releaseAndClaimSynthesis(t, tc, run)

	tc.Pool.processJob(testWorkerID, job)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, failed.Error, "over agent_memory_max_mb (64 MiB)")
}
