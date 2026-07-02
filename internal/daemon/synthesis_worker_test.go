package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

// markMemberRunning transitions a queued member job to running so that the
// status-guarded CompleteJob/FailJob transitions take effect by ID.
func markMemberRunning(t *testing.T, tc *workerTestContext, jobID int64) {
	t.Helper()
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?",
		testWorkerID, jobID,
	)
	require.NoError(t, err)
}

// completeMember drives a specific member to a done review with the given output.
func completeMember(t *testing.T, tc *workerTestContext, jobID int64, ag, output string) {
	t.Helper()
	markMemberRunning(t, tc, jobID)
	require.NoError(t, tc.DB.CompleteJob(jobID, ag, "", output))
}

// failMember drives a specific member to terminal failure.
func failMember(t *testing.T, tc *workerTestContext, jobID int64) {
	t.Helper()
	failMemberWithError(t, tc, jobID, "boom")
}

func failMemberWithError(t *testing.T, tc *workerTestContext, jobID int64, errMsg string) {
	t.Helper()
	markMemberRunning(t, tc, jobID)
	ok, err := tc.DB.FailJob(jobID, testWorkerID, errMsg)
	require.NoError(t, err)
	require.True(t, ok, "FailJob should mark the running member failed")
}

// setSynthesisAgent points the run's synthesis job at a specific agent so tests
// can attach a uniquely-named FakeAgent without clobbering the global "test"
// agent that the rest of the package relies on.
func setSynthesisAgent(t *testing.T, tc *workerTestContext, runUUID, agentName string) {
	t.Helper()
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET agent = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'",
		agentName, runUUID,
	)
	require.NoError(t, err)
}

// releaseAndClaimSynthesis releases the gated synthesis job for the run and
// claims it, asserting the claimed job is the synthesis row.
func releaseAndClaimSynthesis(
	t *testing.T, tc *workerTestContext, runUUID string,
) *storage.ReviewJob {
	t.Helper()
	require.NoError(t, tc.DB.MaybeReleasePanelSynthesis(runUUID))
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed, "expected the released synthesis job to be claimable")
	require.True(t, claimed.IsSynthesisJob(), "claimed job must be the synthesis row")
	return claimed
}

// registerNeverCalledAgent registers a FakeAgent that flips *called and fails
// the test if it is ever invoked. Used to prove the no-agent branches.
func registerNeverCalledAgent(t *testing.T, name string, called *bool) {
	t.Helper()
	agent.Register(&agent.FakeAgent{
		NameStr: name,
		ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
			*called = true
			assert.Fail(t, "synthesis agent must not be invoked in the no-agent branch")
			return "", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(name) })
}

func TestAllMembersPassedIgnoresAllowedFailure(t *testing.T) {
	results := []reviewpkg.ReviewResult{
		{Status: reviewpkg.ResultDone, Output: "No issues found."},
		{Status: reviewpkg.ResultFailed, Error: "pi host disappeared", AllowFailure: true},
	}
	succeeded := filterSucceeded(results)

	assert.True(t, allMembersPassed(results, succeeded))
}

// jobAgentInvoked reads the raw agent_invoked cost-eligibility marker for a job.
func jobAgentInvoked(t *testing.T, tc *workerTestContext, jobID int64) bool {
	t.Helper()
	var invoked int
	require.NoError(t, tc.DB.QueryRow(
		`SELECT agent_invoked FROM review_jobs WHERE id = ?`, jobID).Scan(&invoked))
	return invoked == 1
}

// TestRunSynthesisAgentMarksInvokedOnlyWhenAgentRuns is the regression for the
// pre-agent over-count: a synthesis job whose checkout fails before the agent
// runs must not be marked agent_invoked, while one whose agent actually runs
// must be. The marker moved out of configureSynthesisAgent (which precedes the
// checkout gate) to immediately before each agent call.
func TestRunSynthesisAgentMarksInvokedOnlyWhenAgentRuns(t *testing.T) {
	t.Run("checkout failure is not counted as an agent run", func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		const synthAgent = "synth-checkout-fail"
		registerPassingAgent(t, synthAgent)

		_, _, synth := enqueuePanelRun(t, tc, "checkout-fail-panel", []memberSpec{
			{name: "m0", agent: "test"},
		})
		// The passing agent is not a SynthesisAgent, so the regular Review path
		// runs prepareJobCheckout. Force a CI exact checkout (source=ci) at a head
		// that cannot resolve so the checkout fails before the agent runs, with
		// retries pre-exhausted and no backup so the failure is terminal: FailJob
		// preserves agent_invoked, whereas a requeue would clear it and mask a
		// marker leaked during agent configuration.
		_, err := tc.DB.Exec(
			`UPDATE review_jobs SET status='running', worker_id=?, retry_count=?, agent=?, backup_agent='', source=?, git_ref=? WHERE id=?`,
			testWorkerID, maxRetries, synthAgent, storage.JobSourceCI,
			"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", synth.ID)
		require.NoError(t, err)
		job, err := tc.DB.GetJobByID(synth.ID)
		require.NoError(t, err)

		_, _, runErr := tc.Pool.runSynthesisAgent(context.Background(), testWorkerID, job, "prompt")
		require.Error(t, runErr, "checkout must fail before the agent runs")

		failed, err := tc.DB.GetJobByID(synth.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusFailed, failed.Status, "retries exhausted -> terminal failed")
		assert.False(t, jobAgentInvoked(t, tc, synth.ID),
			"a checkout failure that never ran an agent must not be counted, even at terminal failure")
	})

	t.Run("successful synthesis run is counted", func(t *testing.T) {
		tc := newWorkerTestContext(t, 1)
		const synthAgent = "synth-runs-ok"
		registerPassingAgent(t, synthAgent)

		_, _, synth := enqueuePanelRun(t, tc, "runs-ok-panel", []memberSpec{
			{name: "m0", agent: "test"},
		})
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		_, err := tc.DB.Exec(
			`UPDATE review_jobs SET status='running', worker_id=?, agent=?, source='', git_ref=? WHERE id=?`,
			testWorkerID, synthAgent, sha, synth.ID)
		require.NoError(t, err)
		job, err := tc.DB.GetJobByID(synth.ID)
		require.NoError(t, err)

		_, _, runErr := tc.Pool.runSynthesisAgent(context.Background(), testWorkerID, job, "prompt")
		require.NoError(t, runErr)
		assert.True(t, jobAgentInvoked(t, tc, synth.ID),
			"a synthesis agent that actually runs must be marked agent_invoked")
	})
}

type synthesisEntrypointTestAgent struct {
	name         string
	model        string
	streamLine   string
	result       string
	synthPrompt  string
	reviewCalled bool
}

func (a *synthesisEntrypointTestAgent) Name() string { return a.name }

func (a *synthesisEntrypointTestAgent) Review(context.Context, string, string, string, io.Writer) (string, error) {
	a.reviewCalled = true
	return "", fmt.Errorf("review entrypoint should not be used for synthesis")
}

func (a *synthesisEntrypointTestAgent) Synthesize(ctx context.Context, prompt string, output io.Writer) (string, error) {
	a.synthPrompt = prompt
	if output != nil && a.streamLine != "" {
		if _, err := io.WriteString(output, a.streamLine+"\n"); err != nil {
			return "", err
		}
	}
	if a.result != "" {
		return a.result, nil
	}
	return "## Review Findings\n\n- **Severity**: Medium\n- **Location**: file.go:1\n- **Problem**: combined\n- **Fix**: fix", nil
}

func (a *synthesisEntrypointTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }

func (a *synthesisEntrypointTestAgent) WithAgentic(bool) agent.Agent { return a }

func (a *synthesisEntrypointTestAgent) WithModel(model string) agent.Agent {
	a.model = model
	return a
}

func (a *synthesisEntrypointTestAgent) CommandLine() string { return a.name }

type unavailableSynthesisCommandAgent struct {
	name    string
	command string
}

func (a *unavailableSynthesisCommandAgent) Name() string { return a.name }

func (a *unavailableSynthesisCommandAgent) Review(context.Context, string, string, string, io.Writer) (string, error) {
	return "", fmt.Errorf("unavailable synthesis primary should not run")
}

func (a *unavailableSynthesisCommandAgent) WithReasoning(agent.ReasoningLevel) agent.Agent {
	return a
}

func (a *unavailableSynthesisCommandAgent) WithAgentic(bool) agent.Agent { return a }

func (a *unavailableSynthesisCommandAgent) WithModel(string) agent.Agent { return a }

func (a *unavailableSynthesisCommandAgent) CommandLine() string { return a.command }

func (a *unavailableSynthesisCommandAgent) CommandName() string { return a.command }

func TestHeadOf(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("headsha", headOf("basesha..headsha"), "range returns the head side")
	assert.Equal("plainsha", headOf("plainsha"), "plain ref is unchanged")
	assert.Equal("c", headOf("a..b..c"), "uses the part after the last ..")
	assert.Empty(headOf(""), "empty ref stays empty")
}

func TestConfigureSynthesisAgentUsesStoredBackupWhenPrimaryUnavailable(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const primaryAgent = "synth-primary-unavailable"
	agent.Register(&unavailableSynthesisCommandAgent{
		name:    primaryAgent,
		command: "roborev-test-missing-synthesis-primary",
	})
	t.Cleanup(func() { agent.Unregister(primaryAgent) })

	const backupAgent = "synth-explicit-backup"
	backup := &synthesisEntrypointTestAgent{name: backupAgent}
	agent.Register(backup)
	t.Cleanup(func() { agent.Unregister(backupAgent) })

	_, _, synthJob := enqueuePanelRun(t, tc, "backup-panel", []memberSpec{
		{name: "m0", agent: "test"},
	})
	t.Setenv("PATH", t.TempDir())
	originalCodex, err := agent.Get("codex")
	require.NoError(t, err)
	agent.Register(&agent.FakeAgent{NameStr: "codex"})
	t.Cleanup(func() { agent.Register(originalCodex) })

	_, err = tc.DB.Exec(
		`UPDATE review_jobs
		 SET status = 'running', worker_id = ?, agent = ?, model = ?, backup_agent = ?, backup_model = ?
		 WHERE id = ?`,
		testWorkerID, primaryAgent, "primary-model", backupAgent, "backup-model", synthJob.ID,
	)
	require.NoError(t, err)
	synth, err := tc.DB.GetJobByID(synthJob.ID)
	require.NoError(t, err)
	require.Equal(t, primaryAgent, synth.Agent)
	require.Equal(t, backupAgent, synth.BackupAgent)

	configured, agentName, err := tc.Pool.configureSynthesisAgent(testWorkerID, synth)
	require.NoError(t, err)

	assert.Equal(backupAgent, agentName)
	assert.Equal(backupAgent, configured.Name())
	configuredBackup, ok := configured.(*synthesisEntrypointTestAgent)
	require.True(t, ok)
	assert.Equal("backup-model", configuredBackup.model)
}

func TestConfigureSynthesisAgentKeepsPrimaryModelWhenBackupMatchesPrimary(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const synthAgent = "synth-primary-is-backup"
	synth := &synthesisEntrypointTestAgent{name: synthAgent}
	agent.Register(synth)
	t.Cleanup(func() { agent.Unregister(synthAgent) })

	_, _, synthJob := enqueuePanelRun(t, tc, "same-agent-backup-panel", []memberSpec{
		{name: "m0", agent: "test"},
	})
	_, err := tc.DB.Exec(
		`UPDATE review_jobs
		 SET status = 'running', worker_id = ?, agent = ?, model = ?, backup_agent = ?, backup_model = ?
		 WHERE id = ?`,
		testWorkerID, synthAgent, "primary-model", synthAgent, "backup-model", synthJob.ID,
	)
	require.NoError(t, err)
	job, err := tc.DB.GetJobByID(synthJob.ID)
	require.NoError(t, err)

	configured, agentName, err := tc.Pool.configureSynthesisAgent(testWorkerID, job)
	require.NoError(t, err)

	assert.Equal(synthAgent, agentName)
	assert.Equal(synthAgent, configured.Name())
	configuredSynth, ok := configured.(*synthesisEntrypointTestAgent)
	require.True(t, ok)
	assert.Equal("primary-model", configuredSynth.model)
}

func TestConfigureSynthesisAgentKeepsPrimaryModelForConfiguredACPAlias(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	binDir := t.TempDir()
	const acpCommand = "primary-acp"
	acpBinary := acpCommand
	if runtime.GOOS == "windows" {
		acpBinary += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(binDir, acpBinary), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tc.Pool.cfgGetter.Config().ACP = &config.ACPAgentConfig{
		Name:    "primary-acp",
		Command: acpCommand,
	}

	_, _, synthJob := enqueuePanelRun(t, tc, "configured-acp-panel", []memberSpec{
		{name: "m0", agent: "test"},
	})
	_, err := tc.DB.Exec(
		`UPDATE review_jobs
		 SET status = 'running', worker_id = ?, agent = ?, model = ?, backup_agent = ?, backup_model = ?
		 WHERE id = ?`,
		testWorkerID, "primary-acp", "primary-model", "acp", "backup-model", synthJob.ID,
	)
	require.NoError(t, err)
	job, err := tc.DB.GetJobByID(synthJob.ID)
	require.NoError(t, err)

	configured, agentName, err := tc.Pool.configureSynthesisAgent(testWorkerID, job)
	require.NoError(t, err)

	assert.Equal("acp", agentName)
	configuredACP, ok := configured.(*agent.ACPAgent)
	require.True(t, ok)
	assert.Equal("primary-model", configuredACP.Model)
}

// TestSynthesisAllFailedRendersHeadSHA covers F11: the all-failed review header
// must render the head SHA, never the merge base. processSynthesisJob frames the
// synthesis on the frozen mergeBase..headSHA range, and FormatAllFailedComment
// short-SHAs its argument, so passing the raw range would render the base.
func TestSynthesisAllFailedRendersHeadSHA(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-headsha-member"
	registerPassingAgent(t, memberAgent)
	var synthCalled bool
	const synthAgent = "synth-headsha"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, synth := enqueuePanelRun(t, tc, "headsha-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)

	// Frame the synthesis on a base..head range with distinguishable short SHAs.
	const baseSHA = "1111111aaaaaa"
	const headSHA = "2222222bbbbbb"
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET git_ref = ? WHERE id = ?",
		baseSHA+".."+headSHA, synth.ID,
	)
	require.NoError(t, err)
	for _, m := range members {
		failMember(t, tc, m.ID)
	}

	claimed := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, claimed)

	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Contains(review.Output, "2222222", "all-failed header renders the head short SHA")
	assert.NotContains(review.Output, "1111111", "all-failed header must not render the base SHA")
	assert.False(synthCalled)
}

func TestSynthesisAllFailed(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-all-failed"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "synth-all-failed"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, _ := enqueuePanelRun(t, tc, "all-failed-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	for _, m := range members {
		failMember(t, tc, m.ID)
	}

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Contains(review.Output, "Review Failed")
	assert.Contains(review.Output, "All review jobs in this batch failed")
	assert.False(synthCalled, "no agent should run when every member failed")
}

func TestSynthesisAllQuotaSkippedDoesNotStoreFailReview(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-all-quota-member"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "synth-all-quota"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, _ := enqueuePanelRun(t, tc, "all-quota-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	for _, m := range members {
		failMemberWithError(t, tc, m.ID, reviewpkg.QuotaErrorPrefix+"agent quota exhausted")
	}

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	got := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
	assert.Contains(got.Error, reviewpkg.QuotaErrorPrefix)
	_, err := tc.DB.GetReviewByJobID(synth.ID)
	require.Error(t, err, "all-quota synthesis must not store a verdict-bearing review")
	assert.False(synthCalled, "no agent should run when every member was skipped")
}

func TestSynthesisSingleSuccessPassthrough(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-single"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "synth-single"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, _ := enqueuePanelRun(t, tc, "single-success-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	const memberAOutput = "## Review\nNo issues found."
	completeMember(t, tc, members[0].ID, memberAgent, memberAOutput)
	failMember(t, tc, members[1].ID)

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal(memberAOutput, review.Output, "passthrough must emit member output verbatim")
	assert.Equal("P", storage.ParseVerdict(review.Output), "verdict carried from member output")
	assert.Equal(memberAgent, review.Agent, "review labeled with the surviving member's agent")
	assert.False(synthCalled, "passthrough must not invoke an agent")
}

func TestSynthesisSingleSuccessWithMinSeverityUsesFilterPrompt(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-single-minsev-member"
	registerPassingAgent(t, memberAgent)

	synthAgent := &synthesisEntrypointTestAgent{
		name:   "panel-single-minsev-synth",
		result: "No Medium, High, or Critical findings remain.",
	}
	agent.Register(synthAgent)
	t.Cleanup(func() { agent.Unregister(synthAgent.name) })

	runUUID, members, synthJob := enqueuePanelRun(t, tc, "single-success-minsev-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent.name)
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET min_severity = ? WHERE id = ?",
		"medium", synthJob.ID,
	)
	require.NoError(t, err)
	const memberOutput = "## Review Findings\n\n- Severity: Low\n- Location: low.go:1\n- Problem: low-only\n- Fix: low fix"
	completeMember(t, tc, members[0].ID, memberAgent, memberOutput)
	failMember(t, tc, members[1].ID)

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal("No Medium, High, or Critical findings remain.", review.Output)
	assert.Equal(synthAgent.name, review.Agent)
	assert.False(synthAgent.reviewCalled, "synthesis must use the synthesis entrypoint")
	assert.Contains(synthAgent.synthPrompt, "### Review 1")
	assert.NotContains(synthAgent.synthPrompt, "Agent="+memberAgent)
	assert.NotContains(synthAgent.synthPrompt, "Type=")
	assert.Contains(synthAgent.synthPrompt, "Severity: Low")
	assert.Contains(synthAgent.synthPrompt, "Omit findings below medium severity")
	assert.Contains(synthAgent.synthPrompt, "Only include Medium, High, and Critical findings.")
}

func TestSynthesisSinglePassingSuccessWithMinSeverityPassthrough(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-single-minsev-pass-member"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "panel-single-minsev-pass-synth"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, synthJob := enqueuePanelRun(t, tc, "single-success-minsev-pass-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET min_severity = ? WHERE id = ?",
		"medium", synthJob.ID,
	)
	require.NoError(t, err)
	const memberOutput = "## Review\n\nNo issues found."
	completeMember(t, tc, members[0].ID, memberAgent, memberOutput)
	failMember(t, tc, members[1].ID)

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal(memberOutput, review.Output, "passing single-success output needs no severity filter")
	assert.Equal(memberAgent, review.Agent, "passthrough remains labeled with the surviving member")
	assert.False(synthCalled, "passing single-success min-severity panel must not invoke synthesis")
}

func TestSynthesisSingleMarkerOnlySuccessWithMinSeverityNormalizesOutput(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-single-minsev-marker-member"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "panel-single-minsev-marker-synth"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, synthJob := enqueuePanelRun(t, tc, "single-success-minsev-marker-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET min_severity = ? WHERE id = ?",
		"medium", synthJob.ID,
	)
	require.NoError(t, err)
	completeMember(t, tc, members[0].ID, memberAgent, config.SeverityThresholdMarker)
	failMember(t, tc, members[1].ID)

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal("No issues found.", review.Output)
	assert.Equal(memberAgent, review.Agent, "normalized output remains labeled with the surviving member")
	assert.False(synthCalled, "marker-only single-success min-severity panel must not invoke synthesis")
}

func TestSynthesisAllPassingSkipsAgent(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-all-pass"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "synth-all-pass"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	runUUID, members, _ := enqueuePanelRun(t, tc, "all-pass-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	completeMember(t, tc, members[0].ID, memberAgent, "No issues found.")
	completeMember(t, tc, members[1].ID, memberAgent, "No findings to report.")

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal("No issues found.", review.Output, "stored synthesis output is body content; renderers add headers and footers")
	assert.Equal("P", storage.ParseVerdict(review.Output))
	assert.False(synthCalled, "clean panels must not invoke an extra synthesis agent")
}

func TestSynthesisUsesSynthesisEntrypoint(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-synthesis-entrypoint-member"
	registerPassingAgent(t, memberAgent)

	synthAgent := &synthesisEntrypointTestAgent{name: "panel-synthesis-entrypoint"}
	agent.Register(synthAgent)
	t.Cleanup(func() { agent.Unregister(synthAgent.name) })

	runUUID, members, synth := enqueuePanelRun(t, tc, "entrypoint-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent.name)
	completeMember(t, tc, members[0].ID, memberAgent, "Review #1\n\n## Review Findings\n\n- Severity: Medium\n- Location: a.go:1\n- Problem: A\n- Fix: Fix A")
	completeMember(t, tc, members[1].ID, memberAgent, "Review #2\n\n## Review Findings\n\n- Severity: Medium\n- Location: b.go:2\n- Problem: B\n- Fix: Fix B")

	claimed := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, claimed)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Contains(review.Output, "combined")
	assert.False(synthAgent.reviewCalled, "synthesis must not use the code-review entrypoint")
	assert.Contains(synthAgent.synthPrompt, "Review #1")
	assert.NotContains(synthAgent.synthPrompt, "Review the code changes in commit")
}

func TestSynthesisCapturesTokenUsage(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-synthesis-token-member"
	registerPassingAgent(t, memberAgent)

	synthAgent := &synthesisEntrypointTestAgent{
		name:       "panel-synthesis-token",
		streamLine: `{"type":"thread.started","thread_id":"synth-session-123"}`,
	}
	agent.Register(synthAgent)
	t.Cleanup(func() { agent.Unregister(synthAgent.name) })

	var fetchedSession string
	tc.Pool.tokenUsageFetcher = func(ctx context.Context, sessionID string) (*tokens.Usage, error) {
		fetchedSession = sessionID
		return &tokens.Usage{CostUSD: 0.03, HasCost: true}, nil
	}

	runUUID, members, synth := enqueuePanelRun(t, tc, "synthesis-token-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent.name)
	completeMember(t, tc, members[0].ID, memberAgent, "Review #1\n\n## Review Findings\n\n- Severity: Medium\n- Location: a.go:1\n- Problem: A\n- Fix: Fix A")
	completeMember(t, tc, members[1].ID, memberAgent, "Review #2\n\n## Review Findings\n\n- Severity: Medium\n- Location: b.go:2\n- Problem: B\n- Fix: Fix B")

	claimed := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, claimed)

	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	assert.Equal("synth-session-123", fetchedSession)
	assert.Contains(updated.TokenUsage, `"cost_usd":0.03`)
	assert.Contains(updated.TokenUsage, `"has_cost":true`)
}

func TestSynthesisAutoClosesPassingReview(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		wantClosed bool
	}{
		{name: "enabled", enabled: true, wantClosed: true},
		{name: "disabled", enabled: false, wantClosed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			cfg := config.DefaultConfig()
			cfg.AutoClosePassingReviews = tt.enabled
			tc.reconfigurePool(cfg)

			const memberAgent = "panel-auto-close"
			registerPassingAgent(t, memberAgent)

			runUUID, members, _ := enqueuePanelRun(t, tc, "auto-close", []memberSpec{
				{name: "only", agent: memberAgent},
			})
			completeMember(t, tc, members[0].ID, memberAgent, "No issues found.")

			synth := releaseAndClaimSynthesis(t, tc, runUUID)
			tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

			review, err := tc.DB.GetReviewByJobID(synth.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantClosed, review.Closed)
		})
	}
}

func TestSynthesisMultiVerifyDedupe(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-multi-member"
	registerPassingAgent(t, memberAgent)

	var captured string
	const synthAgent = "synth-multi"
	agent.Register(&agent.FakeAgent{
		NameStr: synthAgent,
		ReviewFn: func(_ context.Context, _, _, prompt string, _ io.Writer) (string, error) {
			captured = prompt
			return "## Combined\nConsolidated finding.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(synthAgent) })

	sub, ch := tc.Broadcaster.Subscribe("")
	defer tc.Broadcaster.Unsubscribe(sub)

	runUUID, members, _ := enqueuePanelRun(t, tc, "multi-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	completeMember(t, tc, members[0].ID, memberAgent, "Finding A in alpha.go")
	completeMember(t, tc, members[1].ID, memberAgent, "Finding B in beta.go")

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	review, err := tc.DB.GetReviewByJobID(synth.ID)
	require.NoError(t, err)
	assert.Equal("## Combined\nConsolidated finding.", review.Output)

	assert.Contains(captured, "Do not call tools or run commands")
	assert.Contains(captured, "Only combine the input review results according to these rules")
	assert.Contains(captured, "Finding A in alpha.go")
	assert.Contains(captured, "Finding B in beta.go")

	assertCompletedBroadcast(t, ch, synth.ID)
}

// assertCompletedBroadcast drains the channel until it sees a review.completed
// event for jobID, failing if none arrives within a short deadline. A deadline
// (rather than a fail-on-first-empty default) keeps the helper correct even if
// the broadcast is not already buffered when it is called.
func assertCompletedBroadcast(t *testing.T, ch <-chan Event, jobID int64) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == "review.completed" && ev.JobID == jobID {
				return
			}
		case <-deadline:
			assert.Fail(t, "expected a review.completed event for the synthesis job")
			return
		}
	}
}

func TestSynthesisPassthroughIgnoresAgentCooldown(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-cooldown-member"
	registerPassingAgent(t, memberAgent)

	var synthCalled bool
	const synthAgent = "synth-cooldown"
	registerNeverCalledAgent(t, synthAgent, &synthCalled)

	// Put the synthesis agent in cooldown — the passthrough branch must ignore
	// it because it never invokes the agent.
	tc.Pool.cooldownAgent(synthAgent, time.Now().Add(time.Hour))

	runUUID, members, _ := enqueuePanelRun(t, tc, "cooldown-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	completeMember(t, tc, members[0].ID, memberAgent, "## Review\nNo issues found.")
	failMember(t, tc, members[1].ID)

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	tc.assertJobStatus(t, synth.ID, storage.JobStatusDone)
	assert.False(synthCalled, "cooldown must not block the no-agent passthrough branch")
}

func TestSynthesisMemberFetchErrorRetries(t *testing.T) {
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-fetch-err"
	registerPassingAgent(t, memberAgent)
	const synthAgent = "synth-fetch-err"
	registerPassingAgent(t, synthAgent)

	runUUID, members, _ := enqueuePanelRun(t, tc, "fetch-err-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	completeMember(t, tc, members[0].ID, memberAgent, "Finding")

	synth := releaseAndClaimSynthesis(t, tc, runUUID)

	// Drop the reviews table so GetPanelMemberReviews errors mid-query while the
	// review_jobs table (and the failover path) stays intact. This drives the
	// real load-error branch rather than mocking it.
	_, err := tc.DB.Exec("DROP TABLE reviews")
	require.NoError(t, err)

	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	// A storage error must retry (queued), never complete the synthesis job as
	// an all-failed review.
	got, err := tc.DB.GetJobByID(synth.ID)
	require.NoError(t, err)
	assert.NotEqual(t, storage.JobStatusDone, got.Status, "load error must not complete the job")
	assert.Equal(t, storage.JobStatusQueued, got.Status, "load error should retry the job")
}

// TestSynthesisMultiSuccessRespectsCooldown verifies the agent-backed synthesis
// branch honors the quota cooldown gate (the no-agent branches intentionally do
// not — see TestSynthesisPassthroughIgnoresAgentCooldown).
func TestSynthesisMultiSuccessRespectsCooldown(t *testing.T) {
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-cd-member"
	registerPassingAgent(t, memberAgent)

	var called bool
	const synthAgent = "synth-cd-multi"
	registerNeverCalledAgent(t, synthAgent, &called)

	// Cool the synthesis agent: the multi-success branch must divert instead of
	// invoking a quota-exhausted agent.
	tc.Pool.cooldownAgent(synthAgent, time.Now().Add(time.Hour))

	runUUID, members, _ := enqueuePanelRun(t, tc, "cd-multi-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	completeMember(t, tc, members[0].ID, memberAgent, "Finding A")
	completeMember(t, tc, members[1].ID, memberAgent, "Finding B")

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	assert.False(t, called, "multi-success synthesis must not invoke an agent in cooldown")
	got, err := tc.DB.GetJobByID(synth.ID)
	require.NoError(t, err)
	assert.NotEqual(t, storage.JobStatusDone, got.Status,
		"cooldown must divert before completing the synthesis")
}

func TestSynthesisCIReviewCooldownDoesNotFailOverToBackup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)

	const memberAgent = "panel-ci-cd-member"
	registerPassingAgent(t, memberAgent)

	var called bool
	const synthAgent = "synth-ci-cd"
	registerNeverCalledAgent(t, synthAgent, &called)

	tc.Pool.cooldownAgent(synthAgent, time.Now().Add(time.Hour))

	runUUID, members, _ := enqueuePanelRun(t, tc, "ci-cd-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	_, err := tc.DB.Exec(
		`UPDATE review_jobs
		 SET source = ?, ci_base_branch = ?, backup_agent = ?
		 WHERE panel_run_uuid = ? AND panel_role = 'synthesis'`,
		storage.JobSourceCI, "main", "test", runUUID,
	)
	require.NoError(t, err)
	completeMember(t, tc, members[0].ID, memberAgent, "Finding A")
	completeMember(t, tc, members[1].ID, memberAgent, "Finding B")

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	assert.False(t, called, "CI synthesis must not invoke an agent in cooldown")
	got := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
	assert.Equal(t, synthAgent, got.Agent, "CI synthesis cooldown must not fail over to backup")
	assert.True(t, strings.HasPrefix(got.Error, reviewpkg.QuotaErrorPrefix),
		"cooldown failure should be a retryable quota skip, got %q", got.Error)
}

// TestSynthesisRunsAgainstWorktree verifies the synthesis agent runs against the
// reviewed checkout: a panel whose synthesis carries a worktree path must hand
// that worktree, not the main repo, to the agent.
func TestSynthesisRunsAgainstWorktree(t *testing.T) {
	assert := assert.New(t)
	tc := newWorkerTestContext(t, 1)

	worktreePath := filepath.Join(t.TempDir(), "wt")
	out, err := exec.Command(
		"git", "-C", tc.TmpDir, "worktree", "add", "--detach", worktreePath, "HEAD",
	).CombinedOutput()
	require.NoError(t, err, "git worktree add failed: %s", out)

	const memberAgent = "panel-wt-member"
	registerPassingAgent(t, memberAgent)

	var capturedPath string
	const synthAgent = "synth-wt"
	agent.Register(&agent.FakeAgent{
		NameStr: synthAgent,
		ReviewFn: func(_ context.Context, repoPath, _, _ string, _ io.Writer) (string, error) {
			capturedPath = repoPath
			return "## Combined\nDone.", nil
		},
	})
	t.Cleanup(func() { agent.Unregister(synthAgent) })

	runUUID, members, _ := enqueuePanelRun(t, tc, "wt-panel", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	_, err = tc.DB.Exec(
		"UPDATE review_jobs SET worktree_path = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'",
		worktreePath, runUUID,
	)
	require.NoError(t, err)

	completeMember(t, tc, members[0].ID, memberAgent, "Finding A")
	completeMember(t, tc, members[1].ID, memberAgent, "Finding B")

	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	require.Equal(t, worktreePath, synth.WorktreePath, "precondition: synthesis carries the worktree")
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	assert.Equal(worktreePath, capturedPath,
		"synthesis agent must run against the reviewed worktree, not the main repo")
}
