package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type blockingScheduledAgent struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingScheduledAgent) Name() string { return "test" }

func (a *blockingScheduledAgent) Review(ctx context.Context, _ string, _ string, _ string, output io.Writer) (string, error) {
	close(a.started)
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.release:
	}
	if output != nil {
		_, _ = io.WriteString(output, "scheduled task complete")
	}
	return "scheduled task complete", nil
}

func (a *blockingScheduledAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *blockingScheduledAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *blockingScheduledAgent) WithModel(string) agent.Agent                   { return a }
func (*blockingScheduledAgent) CommandLine() string                              { return "test" }

func TestScheduledProcessAndPermissionReloadShareOneLock(t *testing.T) {
	previousUnsafe := agent.AllowUnsafeAgents()
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(previousUnsafe) })

	tc := newWorkerTestContext(t, 1)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	initialConfig := "default_agent = \"test\"\n"
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))
	cfg, err := config.LoadGlobalFrom(configPath)
	require.NoError(t, err)
	cw := NewConfigWatcher(configPath, cfg, NewBroadcaster(), nil)
	tc.Pool.cfgGetter = cw

	previous, err := agent.Get("test")
	require.NoError(t, err)
	blocking := &blockingScheduledAgent{started: make(chan struct{}), release: make(chan struct{})}
	agent.Register(blocking)
	t.Cleanup(func() { agent.Register(previous) })

	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID,
		GitRef: sha,
		Agent:  "test",
		Prompt: "scheduled prompt",
		Source: storage.JobSourceScheduled,
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	processDone := make(chan struct{})
	go func() {
		defer close(processDone)
		tc.Pool.processJob(testWorkerID, claimed)
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("scheduled agent did not start")
	}

	require.NoError(t, os.WriteFile(configPath, []byte(
		initialConfig+"allow_unsafe_agents = true\n",
	), 0o600))
	reloadDone := make(chan struct{})
	go func() {
		cw.reloadConfig()
		close(reloadDone)
	}()
	select {
	case <-reloadDone:
		t.Fatal("permission reload acquired the write lock during scheduled execution")
	case <-time.After(25 * time.Millisecond):
	}

	close(blocking.release)
	select {
	case <-processDone:
	case <-time.After(time.Second):
		t.Fatal("scheduled process did not finish")
	}
	select {
	case <-reloadDone:
	case <-time.After(time.Second):
		t.Fatal("permission reload did not finish after scheduled execution")
	}
	assert.True(t, agent.AllowUnsafeAgents())
}
