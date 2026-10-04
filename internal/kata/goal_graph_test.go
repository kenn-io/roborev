package kata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalGraphUnlimitedAndLinks(t *testing.T) {
	rows := make([]map[string]any, 240)
	for i := range rows {
		rows[i] = map[string]any{"short_id": fmt.Sprintf("i%03d", i), "title": "Task", "body": "Full body", "status": "open"}
	}
	rows[0]["parent"] = map[string]string{"short_id": "root", "qualified_id": "project#root"}
	rows[0]["blocked_by"] = []map[string]string{{"short_id": "done", "qualified_id": "other#done", "status": "closed"}}
	rows[0]["related"] = []map[string]string{{"short_id": "peer", "qualified_id": "project#peer"}}
	data, err := json.Marshal(map[string]any{"issues": rows})
	require.NoError(t, err)
	client, args := stubClient(string(data), nil)
	issues, err := client.List(context.Background(), ListOpts{Status: "open", Unlimited: true})
	require.NoError(t, err)
	require.Len(t, issues, 240)
	assert.Equal(t, []string{"list", "--json", "--status", "open", "--limit", "0"}, *args)
	require.NotNil(t, issues[0].Parent)
	assert.Equal(t, "project#root", issues[0].Parent.QualifiedID)
	require.Len(t, issues[0].BlockedBy, 1)
	assert.Equal(t, "other#done", issues[0].BlockedBy[0].QualifiedID)
	require.Len(t, issues[0].Related, 1)
	assert.Equal(t, "Full body", issues[239].Body)
}

func TestGoalGraphShowLinks(t *testing.T) {
	client, _ := stubClient(`{"issue":{"short_id":"abcd","qualified_id":"project#abcd","title":"Task","status":"open"},"links":[{"type":"parent","from":{"short_id":"abcd","qualified_id":"project#abcd"},"to":{"short_id":"root","qualified_id":"project#root"}},{"type":"blocks","from":{"short_id":"peer","qualified_id":"project#peer"},"to":{"short_id":"abcd","qualified_id":"project#abcd"}},{"type":"related","from":{"short_id":"peer","qualified_id":"project#peer"},"to":{"short_id":"abcd","qualified_id":"project#abcd"}}]}`, nil)
	issue, err := client.Show(context.Background(), "abcd")
	require.NoError(t, err)
	require.NotNil(t, issue.Parent)
	assert.Equal(t, "root", issue.Parent.ShortID)
	require.Len(t, issue.BlockedBy, 1)
	assert.Equal(t, "peer", issue.BlockedBy[0].ShortID)
	require.Len(t, issue.Related, 1)
	assert.Equal(t, "peer", issue.Related[0].ShortID)
}

func TestGoalGraphHydratesIncompleteRows(t *testing.T) {
	client := NewCLIClient("")
	var calls [][]string
	client.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		if args[0] == "list" {
			return []byte(`{"issues":[{"short_id":"abcd","title":"Task","status":"open"}]}`), nil
		}
		return []byte(`{"issue":{"short_id":"abcd","qualified_id":"project#abcd","title":"Task","body":"Complete requirement","status":"open"},"links":[{"type":"blocks","from":{"short_id":"abcd","qualified_id":"project#abcd"},"to":{"short_id":"peer","qualified_id":"project#peer"}}]}`), nil
	}
	issues, err := client.List(context.Background(), ListOpts{Unlimited: true})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Complete requirement", issues[0].Body)
	require.Len(t, issues[0].Blocks, 1)
	assert.Equal(t, "peer", issues[0].Blocks[0].ShortID)
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"show", "abcd", "--json"}, calls[1])
}
