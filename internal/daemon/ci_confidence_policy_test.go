package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func TestPanelCommentConfidencePolicy(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	floor := 0.7
	tc.Pool.cfgGetter.Config().CI.CommentMinConfidence = &floor
	for _, mode := range []string{"synthesis", "stored", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			doc := reviewpkg.StructuredReview{SchemaVersion: 3, Summary: "Review complete.", Verdict: "fail", SourceLabels: []string{"first", "second"}, Findings: []reviewpkg.StructuredFinding{
				{Severity: "high", Kind: "defect", Confidence: 0.2, Problem: "Below confidence.", Fix: "Fix it.", Sources: []int{1}},
				{Severity: "high", Kind: "defect", Confidence: 0.8, Problem: "One source.", Fix: "Fix it.", Sources: []int{1}},
				{Severity: "low", Kind: "defect", Confidence: 0.9, Problem: "Below severity.", Fix: "Fix it.", Sources: []int{1, 2}},
				{Severity: "high", Kind: "defect", Confidence: 0.8, Problem: "Two sources.", Fix: "Fix it.", Sources: []int{1, 2}},
			}}
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			a := &synthesisEntrypointTestAgent{name: "confidence-policy", result: string(raw)}
			agent.RegisterForTest(t, a)
			runUUID, members, synthJob := enqueuePanelRun(t, tc, "confidence-policy", []memberSpec{{name: "first", agent: "test"}, {name: "second", agent: "test"}})
			setSynthesisAgent(t, tc, runUUID, a.Name())
			_, err = tc.DB.Exec("UPDATE review_jobs SET min_severity=? WHERE id=?", "high", synthJob.ID)
			require.NoError(t, err)
			for _, m := range members {
				markMemberRunning(t, tc, m.ID)
				require.NoError(t, tc.DB.CompleteJobResult(m.ID, "test", storage.ReviewCompletion{Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}))
			}
			synth := releaseAndClaimSynthesis(t, tc, runUUID)
			if mode == "synthesis" {
				tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
			} else if mode == "stored" {
				// Historical/imported synthesis can retain author order. Publication
				// must rank it without rerunning synthesis or rewriting the stored review.
				require.NoError(t, tc.DB.CompleteJobResult(synth.ID, "test", storage.ReviewCompletion{Output: doc.Markdown(""), StructuredOutput: raw, Verdict: storage.VerdictFail}))
			} else {
				failed, err := tc.DB.FailJob(synth.ID, testWorkerID, "Synthesis unavailable.")
				require.NoError(t, err)
				require.True(t, failed)
			}
			rows, err := tc.DB.GetPanelMemberReviews(runUUID)
			require.NoError(t, err)
			p := &CIPoller{db: tc.DB, cfgGetter: tc.Pool.cfgGetter}
			body, err := p.panelCommentBody(&storage.CIPanel{PanelRunUUID: runUUID, HeadSHA: "abcdef123456", GithubRepo: "example/project"}, rows)
			require.NoError(t, err)
			assert := assert.New(t)
			assert.NotContains(body, "Below severity.")
			assert.Contains(body, "One source.")
			assert.Contains(body, "Two sources.")
			if mode != "fallback" {
				assert.NotContains(body, "Below confidence.")
				assert.Less(strings.Index(body, "Two sources."), strings.Index(body, "One source."))
				if mode == "synthesis" {
					assert.Contains(a.synthPrompt, "Below confidence.")
				}
				stored, err := tc.DB.GetReviewByJobID(synth.ID)
				require.NoError(t, err)
				assert.Contains(stored.Output, "Below confidence.")
				assert.Len(stored.StructuredOutput["findings"], 4)
				assert.Equal(storage.VerdictFail, stored.Verdict())
			} else {
				assert.Contains(body, "Below confidence.")
				assert.Less(strings.Index(body, "One source."), strings.Index(body, "Two sources."))
				assert.Contains(body, "Synthesis unavailable.")
			}
			for _, m := range members {
				stored, err := tc.DB.GetReviewByJobID(m.ID)
				require.NoError(t, err)
				assert.Equal(doc.Markdown(""), stored.Output)
				assert.Equal(storage.VerdictFail, stored.Verdict())
			}
		})
	}
}

func TestResolveCICommentConfidencePolicy(t *testing.T) {
	h := newCIPollerHarness(t, "https://github.com/example/project.git")
	assert.Nil(t, h.Poller.resolveCommentMinConfidence("example/project"))
	floor := 0.7
	h.Cfg.CI.CommentMinConfidence = &floor
	assert.Equal(t, &floor, h.Poller.resolveCommentMinConfidence("example/project"))
	require.NoError(t, os.WriteFile(filepath.Join(h.RepoPath, ".roborev.toml"), []byte("[ci]\ncomment_min_confidence = 0\n"), 0o644))
	assert.Equal(t, new(float64), h.Poller.resolveCommentMinConfidence("example/project"))
	require.NoError(t, os.WriteFile(filepath.Join(h.RepoPath, ".roborev.toml"), []byte("[ci]\ncomment_min_confidence = 2\n"), 0o644))
	assert.Equal(t, &floor, h.Poller.resolveCommentMinConfidence("example/project"), "invalid repository config falls back to global")
}

func TestConfigWatcherRejectsInvalidCommentConfidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	old := config.DefaultConfig()
	floor := 0.7
	old.CI.CommentMinConfidence = &floor
	watcher := NewConfigWatcher(path, old, NewBroadcaster(), nil)
	for _, text := range []string{"-0.1", "1.1", "nan", "+inf", "-inf"} {
		require.NoError(t, os.WriteFile(path, []byte("[ci]\ncomment_min_confidence = "+text+"\n"), 0o600))
		watcher.reloadConfig()
		assert.Same(t, old, watcher.Config())
		assert.Zero(t, watcher.ReloadCounter())
	}
	require.NoError(t, os.WriteFile(path, []byte("[ci]\ncomment_min_confidence = 0\n"), 0o600))
	watcher.reloadConfig()
	assert.Equal(t, new(float64), watcher.Config().CI.CommentMinConfidence)
	assert.EqualValues(t, 1, watcher.ReloadCounter())
}
