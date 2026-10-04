package goalreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

func TestGoalCaptureComplete(t *testing.T) {
	root := t.TempDir()
	writeArtifact(t, root, specPath, "# Feature\n")
	client := &katatest.FakeClient{BindingResult: kata.Binding{Project: "project"}}
	for i := range 240 {
		client.ListResult = append(client.ListResult, kata.Issue{ShortID: fmt.Sprintf("i%03d", i), Title: "Task", Body: "Full requirement", Status: "open", Labels: []string{"roborev"}})
	}
	client.ListResult[0].Parent = &kata.LinkPeer{ShortID: "root", QualifiedID: "other#root", Status: "closed"}
	snapshot, err := Capture(context.Background(), root, Selection{}, client)
	require.NoError(t, err)
	require.Len(t, snapshot.Issues, 240)
	assert.Equal(t, "superpowers", snapshot.Source)
	assert.Equal(t, "spec", snapshot.Stage)
	assert.Equal(t, []kata.ListOpts{{Status: "open", Unlimited: true}}, client.ListOpts)
	assert.Equal(t, []Edge{{Type: "parent", From: "project#i000", To: "other#root"}}, snapshot.Edges)
	assert.Equal(t, "Full requirement", snapshot.Issues[239].Body)
	client.BindingErr = kata.ErrNoBinding
	snapshot, err = Capture(context.Background(), root, Selection{}, client)
	require.NoError(t, err)
	assert.Empty(t, snapshot.Issues)
	client.BindingErr = nil
	client.ListErr = errors.New("ledger unavailable")
	_, err = Capture(context.Background(), root, Selection{}, client)
	require.ErrorContains(t, err, "ledger unavailable")
}

func exampleSnapshot() Snapshot {
	return Snapshot{Source: "superpowers", Stage: "plan", Project: "project", Artifacts: []Artifact{{Kind: "spec", Path: specPath, Content: "# Feature\n"}, {Kind: "plan", Path: planPath, Content: "**Goal:** Feature\n### Task 1: Work\n- [ ] Verify\n```md\n- [ ] example\n```\n"}}, Issues: []kata.Issue{{ShortID: "aaaa", Title: "A", Status: "open", Labels: []string{"z", "a"}}, {ShortID: "bbbb", Title: "B", Status: "open"}}, Edges: []Edge{{Type: "blocks", From: "project#aaaa", To: "project#bbbb"}}}
}

func TestGoalSnapshotIdentity(t *testing.T) {
	original := exampleSnapshot()
	changed := exampleSnapshot()
	slices.Reverse(changed.Issues)
	slices.Reverse(changed.Issues[1].Labels)
	assert.Equal(t, original.ID(), changed.ID())
	changed = exampleSnapshot()
	changed.Artifacts[1].Content = "**Goal:** Feature\n### Task 1: Work\n- [x] Verify\n```md\n- [ ] example\n```\n"
	assert.NotEqual(t, original.ID(), changed.ID())
	assert.Equal(t, original.WatchID([]string{"goal"}), changed.WatchID([]string{"goal"}))
	changed.Artifacts[1].Content += "New requirement\n"
	assert.NotEqual(t, original.WatchID([]string{"goal"}), changed.WatchID([]string{"goal"}))
	assert.Equal(t, original.WatchID([]string{"kata_graph"}), changed.WatchID([]string{"kata_graph"}))
	changed = exampleSnapshot()
	changed.Artifacts[1].Content = "**Goal:** Feature\n### Task 1: Work\n- [ ] Verify\n```md\n- [x] example\n```\n"
	assert.NotEqual(t, original.WatchID([]string{"goal"}), changed.WatchID([]string{"goal"}))
	changed = exampleSnapshot()
	changed.Artifacts[0].Path = "custom.md"
	assert.NotEqual(t, original.ID(), changed.ID())
}

func TestGoalSnapshotWatchIDDropsKataCheckboxStateOutsideCodeFences(t *testing.T) {
	original := exampleSnapshot()
	original.Issues[0].Body = "Checklist:\n- [ ] Run checks\n```markdown\n- [ ] Literal example\n```\n"

	changed := original.clone()
	changed.Issues[0].Body = "Checklist:\n- [x] Run checks\n```markdown\n- [ ] Literal example\n```\n"
	assert.NotEqual(t, original.ID(), changed.ID())
	assert.Equal(t, original.WatchID([]string{"kata_graph"}), changed.WatchID([]string{"kata_graph"}))

	changed = original.clone()
	changed.Issues[0].Body = "Checklist:\n- [ ] Run checks\n```markdown\n- [x] Literal example\n```\n"
	assert.NotEqual(t, original.WatchID([]string{"kata_graph"}), changed.WatchID([]string{"kata_graph"}))

	changed = original.clone()
	changed.Issues[0].Body = "Checklist:\n- [x] Run checks\n```markdown\n- [ ] Literal example\n```\nAnother requirement.\n"
	assert.NotEqual(t, original.WatchID([]string{"kata_graph"}), changed.WatchID([]string{"kata_graph"}))
}

func TestGoalSnapshotEmptyLabelsHaveOneIdentity(t *testing.T) {
	original := exampleSnapshot()
	changed := exampleSnapshot()
	changed.Issues[1].Labels = []string{}
	assert.Equal(t, original.ID(), changed.ID())
	assert.Equal(t, original.WatchID([]string{"kata_graph"}), changed.WatchID([]string{"kata_graph"}))
}

func TestGoalCandidate(t *testing.T) {
	original := exampleSnapshot()
	before := original.ID()
	candidate := Candidate{ShortID: "bbbb", Title: "Updated", Body: "New requirement", Links: []CandidateLink{{Type: "blocks", ToRef: "aaaa"}}}
	updated, err := original.WithCandidate(candidate)
	require.NoError(t, err)
	assert.Equal(t, before, original.ID())
	assert.Equal(t, []Edge{{Type: "blocks", From: "project#bbbb", To: "project#aaaa"}}, updated.Edges)
	assert.Equal(t, "Updated", updated.Issues[1].Title)
	_, err = original.WithCandidate(Candidate{ShortID: "missing", Title: "X", Body: "Y"})
	require.Error(t, err)
	_, err = original.WithCandidate(Candidate{Title: "X", Body: "Y", Links: []CandidateLink{{Type: "blocks", ToRef: "missing"}}})
	require.Error(t, err)
	external := exampleSnapshot()
	external.Edges = append(external.Edges, Edge{Type: "related", From: "project#aaaa", To: "other#closed"})
	_, err = external.WithCandidate(Candidate{Title: "X", Body: "Y", Links: []CandidateLink{{Type: "related", ToRef: "other#closed"}}})
	require.Error(t, err, "capture-only external refs are not writable candidate targets")
	created, err := original.WithCandidate(Candidate{Title: "New", Body: "Requirement"})
	require.NoError(t, err)
	require.Len(t, created.Issues, 3)
	assert.Equal(t, "proposed", created.Issues[2].ShortID)
	preserved, err := original.WithCandidate(Candidate{ShortID: "aaaa", Title: "A", Body: "Body"})
	require.NoError(t, err)
	assert.Equal(t, original.Edges, preserved.Edges)
	assert.ElementsMatch(t, original.Issues[0].Labels, preserved.Issues[0].Labels)
	cleared, err := original.WithCandidate(Candidate{ShortID: "aaaa", Title: "A", Body: "Body", Labels: []string{}, Links: []CandidateLink{}})
	require.NoError(t, err)
	assert.Empty(t, cleared.Edges)
	assert.Empty(t, cleared.Issues[0].Labels)
}

func FuzzSnapshot(f *testing.F) {
	f.Add("Requirement", "plan")
	f.Fuzz(func(t *testing.T, body, stage string) {
		snapshot := exampleSnapshot()
		snapshot.Issues[0].Body = body
		snapshot.Stage = stage
		reordered := snapshot.clone()
		slices.Reverse(reordered.Issues)
		assert.Equal(t, snapshot.ID(), reordered.ID())
	})
}

func TestGoalCandidateJSONPreservesExplicitEmpty(t *testing.T) {
	candidate := Candidate{ShortID: "aaaa", Title: "A", Body: "Requirement", Labels: []string{}, Links: []CandidateLink{}}
	data, err := json.Marshal(candidate)
	require.NoError(t, err)
	var decoded Candidate
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.Labels)
	require.NotNil(t, decoded.Links)
	updated, err := exampleSnapshot().WithCandidate(decoded)
	require.NoError(t, err)
	assert.Empty(t, updated.Edges)
	assert.Empty(t, updated.Issues[0].Labels)
}

func TestGoalCapturePreservesQualifiedIdentities(t *testing.T) {
	root := t.TempDir()
	writeArtifact(t, root, specPath, "# Feature\n")
	client := &katatest.FakeClient{BindingResult: kata.Binding{Project: "alias"}, ListResult: []kata.Issue{
		{ShortID: "aaaa", QualifiedID: "project#aaaa", Title: "A", Status: "open", Blocks: []kata.LinkPeer{{ShortID: "bbbb", QualifiedID: "project#bbbb"}}},
		{ShortID: "bbbb", QualifiedID: "project#bbbb", Title: "B", Status: "open"},
	}}
	snapshot, err := Capture(context.Background(), root, Selection{}, client)
	require.NoError(t, err)
	assert.Equal(t, "project#aaaa", snapshot.Issues[0].QualifiedID)
	assert.Equal(t, []Edge{{Type: "blocks", From: "project#aaaa", To: "project#bbbb"}}, snapshot.Edges)
	updated, err := snapshot.WithCandidate(Candidate{ShortID: "bbbb", Title: "B", Body: "Requirement", Links: []CandidateLink{{Type: "related", ToRef: "aaaa"}}})
	require.NoError(t, err)
	assert.Equal(t, []Edge{{Type: "related", From: "project#aaaa", To: "project#bbbb"}}, updated.Edges)
}

type changingGoalClient struct {
	katatest.FakeClient
	change func()
}

func (c *changingGoalClient) List(ctx context.Context, opts kata.ListOpts) ([]kata.Issue, error) {
	c.change()
	return c.FakeClient.List(ctx, opts)
}

func TestGoalCaptureRetriesChangingArtifacts(t *testing.T) {
	for _, continual := range []bool{false, true} {
		t.Run(fmt.Sprint(continual), func(t *testing.T) {
			root := t.TempDir()
			writeArtifact(t, root, specPath, "# Original\n")
			calls := 0
			client := &changingGoalClient{BindingResult: kata.Binding{Project: "project"}}
			client.change = func() {
				calls++
				if continual || calls == 1 {
					writeArtifact(t, root, specPath, fmt.Sprintf("# Revision %d\n", calls))
				}
			}
			snapshot, err := Capture(context.Background(), root, Selection{}, client)
			if continual {
				require.ErrorContains(t, err, "changed during capture")
				assert.Equal(t, 3, calls, "changing inputs cannot retry without a bound")
			} else {
				require.NoError(t, err)
				assert.Equal(t, 2, calls)
				assert.Equal(t, "# Revision 1\n", snapshot.Artifacts[0].Content)
			}
		})
	}
}
