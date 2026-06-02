package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// TestFetchPanelMembersRequestsFullRun verifies show requests the full member
// list (limit=0) so a panel with >=50 rows is not truncated, and that the
// synthesis row is filtered out of the returned members.
func TestFetchPanelMembersRequestsFullRun(t *testing.T) {
	assert := assert.New(t)
	var gotLimit, gotPanelRun string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/jobs", r.URL.Path)
		gotLimit = r.URL.Query().Get("limit")
		gotPanelRun = r.URL.Query().Get("panel_run")
		writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{
			{ID: 41, PanelRole: storage.PanelRoleMember, PanelMemberIndex: 1},
			{ID: 40, PanelRole: storage.PanelRoleMember, PanelMemberIndex: 0},
			{ID: 99, PanelRole: storage.PanelRoleSynthesis},
		}})
	}))
	defer server.Close()

	members, err := fetchPanelMembers(server.Client(), server.URL, "run-uuid-1")
	require.NoError(t, err)

	assert.Equal("0", gotLimit, "show must request the full run (limit=0)")
	assert.Equal("run-uuid-1", gotPanelRun)
	// Synthesis row excluded; members ordered by panel_member_index.
	require.Len(t, members, 2)
	assert.Equal(int64(40), members[0].ID)
	assert.Equal(int64(41), members[1].ID)
}

func sampleMembers() []storage.ReviewJob {
	return []storage.ReviewJob{
		{ID: 40, PanelMemberName: "bug", Agent: "codex", ReviewType: "", Status: storage.JobStatusDone, Verdict: new("P")},
		{ID: 41, PanelMemberName: "security", Agent: "codex", ReviewType: "security", Status: storage.JobStatusDone, Verdict: new("F")},
		{ID: 42, PanelMemberName: "design", Agent: "claude-code", ReviewType: "design", Status: storage.JobStatusFailed, Verdict: nil},
	}
}

func TestBuildShowPanelBlock(t *testing.T) {
	assert := assert.New(t)

	block := buildShowPanelBlock(99, "run-uuid-1", "branch_final", sampleMembers())

	assert.Equal("run-uuid-1", block.RunUUID)
	assert.Equal("branch_final", block.Name)
	assert.Equal(int64(99), block.SynthesisJobID)
	assert.Len(block.Members, 3)

	assert.Equal(int64(40), block.Members[0].JobID)
	assert.Equal("bug", block.Members[0].Name)
	assert.Equal("codex", block.Members[0].Agent)
	assert.Equal("P", block.Members[0].Verdict)
	assert.Equal("done", block.Members[0].Status)

	assert.Equal("F", block.Members[1].Verdict)
	assert.Equal("security", block.Members[1].ReviewType)

	// A member with no verdict yet serializes an empty verdict.
	assert.Empty(block.Members[2].Verdict)
	assert.Equal("failed", block.Members[2].Status)
}

func TestFormatReviewersSummary(t *testing.T) {
	assert := assert.New(t)

	got := formatReviewersSummary(sampleMembers())
	assert.Equal("3 reviewers: bug P, security F, design -", got)
}

// TestFormatReviewersSummaryFallsBackToAgent verifies a member with no panel
// member name is labeled by its agent instead.
func TestFormatReviewersSummaryFallsBackToAgent(t *testing.T) {
	assert := assert.New(t)

	members := []storage.ReviewJob{
		{ID: 1, PanelMemberName: "", Agent: "codex", Status: storage.JobStatusDone, Verdict: new("P")},
	}
	assert.Equal("1 reviewers: codex P", formatReviewersSummary(members))
}
