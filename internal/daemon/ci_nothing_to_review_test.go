package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

// A PR whose changes are all excluded gives every member an empty diff, and
// an agent shown an empty diff reports no issues. The poller publishes a skip
// for that PR instead of a passing review. A PR with code still gets a panel.
func TestCIPollerSkipsPRWithNothingToReview(t *testing.T) {
	for _, tc := range []struct {
		name      string
		file      string
		wantPanel bool
	}{
		{name: "only excluded files", file: ".cache/build.txt"},
		{name: "code change", file: "main.go", wantPanel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.Cfg.CI.Agents = []string{"codex"}
			h.Cfg.CI.ReviewTypes = []string{"default"}
			h.stubProcessPRGit()
			repo := testutil.InitTestGitRepo(t, h.RepoPath)
			base := repo.HeadSHA()
			head := repo.CommitFile(tc.file, "package main\n", "change")
			h.Poller.mergeBaseFn = func(string, string, string) (string, error) { return base, nil }
			checks, statuses := h.CaptureSkippedChecks(), h.CaptureCommitStatuses()

			require.NoError(t, h.Poller.processPR(t.Context(), "acme/api", ghPR{
				Number: 5, HeadRefOid: head, BaseRefName: "main",
			}, h.Cfg))

			assert.Equal(t, tc.wantPanel, h.hasPanel(t, "acme/api", 5, head))
			if tc.wantPanel {
				assert.Empty(t, *checks)
				return
			}
			assert.Equal(t, []capturedSkippedCheck{{
				Repo: "acme/api", SHA: head, Summary: ciNothingToReviewSkipReason,
			}}, *checks)
			for _, status := range *statuses {
				assert.NotEqual(t, "failure", status.State)
				assert.NotEqual(t, "error", status.State)
			}
		})
	}
}
