package client

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestGoalCandidateWirePreservesClearing(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		clear         bool
	}{
		{"preserve", `{"short_id":"aaaa","title":"Updated","body":"Requirement"}`, false},
		{"clear", `{"short_id":"aaaa","title":"Updated","body":"Requirement","labels":[],"links":[]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			var candidate generated.Candidate
			require.NoError(t, json.Unmarshal([]byte(tt.payload), &candidate))
			wire, err := json.Marshal(candidate)
			require.NoError(t, err)
			var native goalreview.Candidate
			require.NoError(t, json.Unmarshal(wire, &native))
			snapshot := goalreview.Snapshot{
				Source: "superpowers", Stage: "spec", Project: "project",
				Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}},
				Issues:    []kata.Issue{{ShortID: "aaaa", Title: "Task", Body: "Requirement", Status: "open", Labels: []string{"existing"}}, {ShortID: "bbbb", Title: "Peer", Status: "open"}},
				Edges:     []goalreview.Edge{{Type: "related", From: "project#aaaa", To: "project#bbbb"}},
			}
			updated, err := snapshot.WithCandidate(native)
			require.NoError(t, err)
			if tt.clear {
				assert.Empty(updated.Issues[0].Labels, "explicit empty labels must survive the generated client's JSON encoding")
				assert.Empty(updated.Edges, "explicit empty links must remove incident relations")
			} else {
				assert.Equal(snapshot.Issues[0].Labels, updated.Issues[0].Labels)
				assert.Equal(snapshot.Edges, updated.Edges)
			}
		})
	}
}
