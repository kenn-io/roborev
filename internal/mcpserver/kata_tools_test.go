package mcpserver

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

// A default MCP client may inspect finding claims, but cannot silently gain
// the ability to publish typed replies through the daemon's credential.
func TestKataToolsRequireExplicitWriteOptIn(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit opt in"}[allow], func(t *testing.T) {
			var options []Option
			if allow {
				options = append(options, WithKataWrites())
			}
			session := connectServer(t, New(&fakeBackend{}, "test", nil, options...))
			listed, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			names := make([]string, 0, len(listed.Tools))
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			assert.Contains(t, names, "roborev_kata_issues")
			assert.Contains(t, names, "roborev_kata_context")
			if allow {
				assert.Contains(t, names, "roborev_kata_reply")
			} else {
				assert.NotContains(t, names, "roborev_kata_reply")
				_, err := session.CallTool(t.Context(), &mcp.CallToolParams{
					Name: "roborev_kata_reply", Arguments: map[string]any{"job_id": 1},
				})
				require.Error(t, err, "an unadvertised write tool must also be uncallable")
			}
		})
	}
}

func TestOptedInStdioKataReplyUsesDurableDaemonAPI(t *testing.T) {
	d, backend := newFakeDaemon(t)
	selector, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
	require.NoError(t, err)
	intent := kataresponse.Request{JobID: 7, OperationID: "Readback-Case-Key", FindingID: selector.String(), Kind: "reply", Body: "Committed a proposed fix; validate it independently."}
	calls := 0
	d.routes["/api/kata/response"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodPost, r.Method)
		var got kataresponse.Request
		assert.NoError(t, json.UnmarshalRead(r.Body, &got))
		assert.Equal(t, intent, got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		assert.NoError(t, json.MarshalWrite(w, storage.KataResponseStatus{OperationID: intent.OperationID, Kind: intent.Kind, State: "pending", Target: "root-example"}))
	}
	session := connectServer(t, New(backend, "test", nil, WithKataWrites()))
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "roborev_kata_reply", Arguments: map[string]any{
		"job_id": intent.JobID, "operation_id": intent.OperationID, "finding_id": intent.FindingID, "kind": intent.Kind, "body": intent.Body,
	}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, 1, calls)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var status storage.KataResponseStatus
	require.NoError(t, json.Unmarshal(data, &status))
	assert.Equal(t, "pending", status.State)
	assert.Equal(t, "root-example", status.Target)
}
