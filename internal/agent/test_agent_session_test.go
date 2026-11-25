package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTestAgent_FreshCallsEmitDistinctSessionIDs(t *testing.T) {
	a := NewTestAgent()

	var buf1 bytes.Buffer
	_, err := a.Review(t.Context(), "/repo", "abc", "prompt 1", &buf1)
	require.NoError(t, err)
	id1 := ExtractSessionID(buf1.String())
	assert.Equal(t, "test-session-1", id1)

	var buf2 bytes.Buffer
	_, err = a.Review(t.Context(), "/repo", "def", "prompt 2", &buf2)
	require.NoError(t, err)
	id2 := ExtractSessionID(buf2.String())
	assert.Equal(t, "test-session-2", id2)

	calls := a.Calls()
	require.Len(t, calls, 2)
	assert.Empty(t, calls[0].SessionID, "first call had no incoming session")
	assert.Empty(t, calls[1].SessionID, "second call had no incoming session")
}

func TestTestAgent_WithSessionIDEchoesReceivedID(t *testing.T) {
	a := NewTestAgent()
	resumed := a.WithSessionID("test-session-1").(*TestAgent)

	var buf bytes.Buffer
	_, err := resumed.Review(t.Context(), "/repo", "abc", "prompt", &buf)
	require.NoError(t, err)

	id := ExtractSessionID(buf.String())
	assert.Equal(t, "test-session-1", id, "resumed call echoes the incoming session ID")

	calls := resumed.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "test-session-1", calls[0].SessionID)
}

func TestTestAgent_NewInstanceResetsCounter(t *testing.T) {
	a1 := NewTestAgent()
	var buf bytes.Buffer
	_, err := a1.Review(t.Context(), "/repo", "abc", "p", &buf)
	require.NoError(t, err)
	assert.Equal(t, "test-session-1", ExtractSessionID(buf.String()))

	a2 := NewTestAgent()
	buf.Reset()
	_, err = a2.Review(t.Context(), "/repo", "abc", "p", &buf)
	require.NoError(t, err)
	assert.Equal(t, "test-session-1", ExtractSessionID(buf.String()),
		"new instance starts its own counter at 1")
}
