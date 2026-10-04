package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestLocalSynthesisCheckoutSurvivesCallerRemoval(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	tc.Pool.cfgGetter.Config().IsolateReviews = true
	tc.GitRepo.CommitFile("marker.txt", "reviewed\n", "head")
	caller := filepath.Join(t.TempDir(), "caller")
	tc.GitRepo.Run("worktree", "add", "--detach", caller, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(caller, ".roborev.toml"), []byte("max_prompt_size=4096\nsnapshot_dir='review-snapshots'"), 0o600))
	started := make(chan string, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	const synthAgent = "isolated-synthesis-reader"
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: synthAgent, ReviewFn: func(ctx context.Context, path, ref, prompt string, out io.Writer) (string, error) {
		if _, err := io.WriteString(out, "{\"session_id\":\"fresh-synthesis-session\"}\n"); err != nil {
			return "", err
		}
		started <- path
		<-release
		data, err := os.ReadFile(filepath.Join(path, "marker.txt"))
		if err != nil {
			return "", err
		}
		if string(data) != "reviewed\n" {
			return "", fmt.Errorf("wrong synthesis checkout")
		}
		files, err := filepath.Glob(filepath.Join(path, "review-snapshots", "*", "prompt.md"))
		if err != nil {
			return "", err
		}
		if len(files) != 1 {
			return "", fmt.Errorf("expected one synthesis snapshot")
		}
		data, err = os.ReadFile(files[0])
		if err != nil {
			return "", err
		}
		if !strings.Contains(string(data), "member finding") {
			return "", fmt.Errorf("missing member review input")
		}
		return `{"schema_version":2,"summary":"Done.","verdict":"pass","findings":[]}`, nil
	}})
	run, members, _ := enqueuePanelRun(t, tc, "isolated-panel", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
	setSynthesisAgent(t, tc, run, synthAgent)
	_, err := tc.DB.Exec("UPDATE review_jobs SET worktree_path=? WHERE panel_run_uuid=?", caller, run)
	require.NoError(t, err)
	completeMember(t, tc, members[0].ID, "test", strings.Repeat("member finding ", 1000))
	completeMember(t, tc, members[1].ID, "test", strings.Repeat("another member finding ", 1000))
	synth := releaseAndClaimSynthesis(t, tc, run)
	_, err = tc.DB.Exec("UPDATE review_jobs SET session_id='old-synthesis-session',session_resumed=1,resume_source_job_uuid=uuid WHERE id=?", synth.ID)
	require.NoError(t, err)
	synth.SessionID = "old-synthesis-session"
	go func() { defer close(done); tc.Pool.processJob(testWorkerID, synth) }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
		}
	})
	var path string
	select {
	case path = <-started:
	case <-done:
	case <-time.After(2 * time.Minute):
	}
	require.NotEmpty(t, path, "synthesis must start")
	require.NotEqual(t, caller, path)
	tc.GitRepo.Run("worktree", "remove", "--force", caller)
	close(release)
	finished := false
	select {
	case <-done:
		finished = true
	case <-time.After(2 * time.Minute):
	}
	require.True(t, finished, "synthesis must finish")
	got, err := tc.DB.GetJobByID(synth.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, got.Status)
	assert.Equal(t, "fresh-synthesis-session", got.SessionID)
	assert.Nil(t, got.ResumeSourceJobUUID)
	var resumed int
	require.NoError(t, tc.DB.QueryRow("SELECT session_resumed FROM review_jobs WHERE id=?", synth.ID).Scan(&resumed))
	assert.Zero(t, resumed)
	assert.Equal(t, caller, got.WorktreePath)
	assert.NoDirExists(t, path)
}
