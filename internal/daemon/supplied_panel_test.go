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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestSuppliedPanelKeepsInputsThroughExecutionAndRerun(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	base := repo.CommitFile("file.txt", "before\n", "base")
	repo.CommitFile(".roborev.toml", "exclude_patterns = ['file.txt']\nreview_min_severity = 'high'\nmodel = 'checkout-model'\nmax_prompt_size = 1\nsnapshot_dir = 'checkout-snapshots'\n", "policy")
	head := repo.CommitFile("file.txt", "VISIBLE_CHANGE\n", "target")
	ref := base + ".." + head
	memberCalls, synthesisCalls := 0, 0
	memberPrompts := make(map[string]int)
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-panel-member", ReviewFn: func(_ context.Context, root, gotRef, text string, _ io.Writer) (string, error) {
		memberCalls++
		assert.Equal(t, ref, gotRef)
		assert.Contains(t, text, "CALLER_REVIEW_POLICY")
		for _, name := range []string{"general", "correctness"} {
			if strings.Contains(text, "CALLER_REVIEW_POLICY_"+name) {
				memberPrompts[name]++
			}
		}
		matches, err := filepath.Glob(filepath.Join(root, ".roborev", "*", "roborev-snapshot-content.diff"))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		data, err := os.ReadFile(matches[0])
		require.NoError(t, err)
		assert.Contains(t, string(data), "+VISIBLE_CHANGE")
		return `{"schema_version":3,"summary":"Check missing","verdict":"fail","findings":[{"severity":"low","kind":"defect","confidence":0.8,"problem":"Missing check.","fix":"Add check.","file":null,"start_line":null,"end_line":null}]}`, nil
	}})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "supplied-panel-synthesis", ReviewFn: func(_ context.Context, root, gotRef, text string, _ io.Writer) (string, error) {
		synthesisCalls++
		assert.Equal(t, ref, gotRef)
		assert.Contains(t, text, "CALLER_SYNTHESIS_POLICY")
		data, err := os.ReadFile(filepath.Join(root, "file.txt"))
		require.NoError(t, err)
		assert.Equal(t, "VISIBLE_CHANGE\n", string(data))
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})
	httpServer := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(httpServer.Close)
	client, err := roborevclient.New(httpServer.URL)
	require.NoError(t, err)
	panel, err := client.EnqueueSuppliedPanel(t.Context(), &generated.EnqueueSuppliedPanelRequestOptions{
		Body: &generated.EnqueueSuppliedPanelInputBody{
			RepoPath: repo.Path(), GitRef: ref, HoldSynthesis: new(true),
			Members: []generated.SuppliedPanelMember{
				{Name: "general", Agent: "supplied-panel-member", Model: new("caller-model"), ReviewPrompt: "CALLER_REVIEW_POLICY_general: " + prompt.DiffFilePathPlaceholder},
				{Name: "correctness", Agent: "supplied-panel-member", Model: new("caller-model"), ReviewPrompt: "CALLER_REVIEW_POLICY_correctness: " + prompt.DiffFilePathPlaceholder},
			},
			Synthesis: generated.SuppliedPanelSynthesis{Agent: "supplied-panel-synthesis", Model: new("synthesis-model"), Instructions: new("CALLER_SYNTHESIS_POLICY")},
		},
	})
	require.NoError(t, err)
	require.Len(t, panel.MemberJobIds, 2)
	for run := range 2 {
		for range 2 {
			job, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, job)
			require.Equal(t, storage.PanelRoleMember, job.PanelRole)
			assert.Equal(t, "caller-model", job.Model)
			server.workerPool.processJob(testWorkerID, job)
			finished, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			require.NotNil(t, review.VerdictBool)
			assert.Equal(t, 0, *review.VerdictBool, "checkout severity must not hide the finding")
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, reports, 1)
			assert.Equal(t, "succeeded", reports[0].Status)
		}
		job, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		assert.Nil(t, job)
		w := requestSynthesis(t, server, panel.PanelRunUUID)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		job, err = db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, job)
		assert.Equal(t, "synthesis-model", job.Model)
		server.workerPool.processJob(testWorkerID, job)
		finished, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
		if run == 0 {
			// Even malformed current policy must not change the frozen rerun.
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("invalid TOML ["), 0o600))
			rerun, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: panel.ID}})
			require.NoError(t, err)
			panel.PanelRunUUID = *rerun.Body.RunUUID
			panel.ID = rerun.Body.JobID
		}
	}
	assert.Equal(t, 4, memberCalls)
	assert.Equal(t, 2, synthesisCalls)
	assert.Equal(t, map[string]int{"general": 2, "correctness": 2}, memberPrompts)
}

func TestSuppliedPanelPreparesSynthesisTools(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target", "target")
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "strict-panel-member", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
	}})
	calls := 0
	agent.RegisterForTest(t, &strictWorkerTestAgent{
		structuredWorkerTestAgent: &structuredWorkerTestAgent{name: "strict-panel-synthesis", result: jsontext.Value(testutil.ReviewFixtureJSON("No issues found."))},
		onReview: func(path string) {
			calls++
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.JSONEq(t, `{"mcpServers":{}}`, string(data))
		},
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": head,
		"members":   []any{map[string]any{"name": "reviewer", "agent": "strict-panel-member", "review_prompt": "Review.", "review_settings": map[string]any{}}},
		"synthesis": map[string]any{"agent": "strict-panel-synthesis", "instructions": "Verify the finding.", "tools": map[string]any{"mode": "read-only", "mcp": map[string]any{}}},
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	for range 2 {
		job, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, job)
		server.workerPool.processJob(testWorkerID, job)
		finished, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
	}
	assert.Equal(t, 1, calls)
}

func TestSuppliedPanelRestoresPrimaryAgentsAfterFailover(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target\n", "target")
	calls := make(map[string]int)
	for _, name := range []string{"panel-primary", "panel-backup", "synthesis-primary", "synthesis-backup"} {
		agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			calls[name]++
			if name == "panel-primary" || name == "synthesis-primary" {
				return "partial", errors.New("context window exceeded")
			}
			if name == "panel-backup" {
				return string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")), nil
			}
			return string(testutil.ReviewFixtureJSON("No issues found.")), nil
		}})
	}
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": head,
		"members": []any{map[string]any{
			"name": "reviewer", "agent": "panel-primary", "model": "member-primary-model", "review_prompt": "Review the change.",
			"review_settings": map[string]any{"backup_agent": "panel-backup", "backup_model": "member-backup-model"},
		}},
		"synthesis": map[string]any{"agent": "synthesis-primary", "model": "synthesis-primary-model", "backup_agent": "synthesis-backup", "backup_model": "synthesis-backup-model", "instructions": "Verify the review."},
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var panel PanelEnqueueResponse
	testutil.DecodeJSON(t, w, &panel)
	for run := range 2 {
		for _, step := range []struct{ agent, model string }{
			{"panel-primary", "member-primary-model"},
			{"panel-backup", "member-backup-model"},
			{"synthesis-primary", "synthesis-primary-model"},
			{"synthesis-backup", "synthesis-backup-model"},
		} {
			job, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, job)
			require.Equal(t, step.agent, job.Agent)
			assert.Equal(t, step.model, job.Model)
			server.workerPool.processJob(testWorkerID, job)
		}
		finished, err := db.GetJobByID(panel.ID)
		require.NoError(t, err)
		require.Equal(t, storage.JobStatusDone, finished.Status, finished.Error)
		if run == 0 {
			rerun, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: panel.ID}})
			require.NoError(t, err)
			panel.ID = rerun.Body.JobID
		}
	}
	assert.Equal(t, map[string]int{"panel-primary": 2, "panel-backup": 2, "synthesis-primary": 2, "synthesis-backup": 2}, calls)
}

func TestSuppliedPanelRejectsInvalidPlanWithoutQueueingMembers(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target", "target")
	for _, tc := range []struct {
		name   string
		mutate func(*EnqueueSuppliedPanelInput)
	}{
		{"last member agent", func(in *EnqueueSuppliedPanelInput) { in.Body.Members[1].Agent = "no-such-agent" }},
		{"duplicate name", func(in *EnqueueSuppliedPanelInput) { in.Body.Members[1].Name = "first" }},
		{"empty prompt", func(in *EnqueueSuppliedPanelInput) { in.Body.Members[1].ReviewPrompt = " " }},
		{"mutable context", func(in *EnqueueSuppliedPanelInput) {
			in.Body.Members[1].ReviewPrompt = prompt.PriorRangeReviewsFilePathPlaceholder
		}},
		{"synthesis agent", func(in *EnqueueSuppliedPanelInput) { in.Body.Synthesis.Agent = "no-such-agent" }},
		{"synthesis unsupported tools", func(in *EnqueueSuppliedPanelInput) {
			in.Body.Synthesis.Tools = &config.ReviewToolPolicy{Mode: "read-only"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in EnqueueSuppliedPanelInput
			in.Body.RepoPath, in.Body.GitRef = repo.Path(), head
			for _, name := range []string{"first", "second"} {
				in.Body.Members = append(in.Body.Members, SuppliedPanelMember{Agent: "test", Name: name, ReviewPrompt: "Review."})
			}
			in.Body.Synthesis.Agent = "test"
			tc.mutate(&in)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", in.Body))
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			jobs, err := db.ListJobs("", "", 10, 0)
			require.NoError(t, err)
			assert.Empty(t, jobs)
		})
	}
}
