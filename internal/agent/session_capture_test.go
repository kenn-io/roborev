package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractSessionID(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "claude init session_id",
			line: `{"type":"system","subtype":"init","session_id":"claude-session-123"}`,
			want: "claude-session-123",
		},
		{
			name: "camel sessionId",
			line: `{"type":"init","sessionId":"camel-session-456"}`,
			want: "camel-session-456",
		},
		{
			name: "pascal sessionID",
			line: `{"type":"text","sessionID":"pascal-session-999"}`,
			want: "pascal-session-999",
		},
		{
			name: "agy conversation_id",
			line: `{"event":"init","conversation_id":"agy-conv-123"}`,
			want: "agy-conv-123",
		},
		{
			name: "undocumented camel conversationId",
			line: `{"event":"init","conversationId":"agy-camel-456"}`,
			want: "",
		},
		{
			name: "agy result event conversation_id is ignored",
			line: `{"event":"result","conversation_id":"agy-res-789","response":"ok"}`,
			want: "",
		},
		{
			name: "conversation_id outside an init event is ignored",
			line: `{"event":"step_update","conversation_id":"agy-step-789"}`,
			want: "",
		},
		{
			name: "pi session event id",
			line: `{"type":"session","id":"pi-session-321"}`,
			want: "pi-session-321",
		},
		{
			name: "codex thread started",
			line: `{"type":"thread.started","thread_id":"thread-789"}`,
			want: "thread-789",
		},
		{
			name: "other thread id ignored",
			line: `{"type":"turn.started","thread_id":"thread-789"}`,
			want: "",
		},
		{
			name: "plain text ignored",
			line: "not json",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractSessionID(tc.line))
		})
	}
}
