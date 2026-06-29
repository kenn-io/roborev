package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

func TestDiscordFailureClass(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want string
	}{
		{
			name: "quota cooldown",
			err:  review.QuotaErrorPrefix + "agent codex quota cooldown active",
			want: "quota/cooldown",
		},
		{
			name: "provider outage",
			err:  review.OutageErrorPrefix + "429 too many requests",
			want: "provider/session outage",
		},
		{
			name: "agent timeout",
			err:  agentTimeoutErrorPrefix + " 30m0s",
			want: "timeout",
		},
		{
			name: "generic",
			err:  "agent: model not found",
			want: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := storage.ReviewJob{
				Status: storage.JobStatusFailed,
				Error:  tt.err,
			}
			assert.Equal(t, tt.want, discordFailureClass(job, ""))
		})
	}
}

func TestCanonicalDiscordAgentUsesQuotaCooldownAgent(t *testing.T) {
	job := storage.ReviewJob{
		Agent: "synthesis",
		Error: review.QuotaErrorPrefix + "agent codex quota cooldown active",
	}
	event := Event{Agent: "synthesis"}

	assert.Equal(t, "codex", canonicalDiscordAgent(job, event))
}

func TestBuildDiscordCIJobFailedPayloadIncludesContext(t *testing.T) {
	job := storage.ReviewJob{
		ID:              42,
		RepoName:        "api",
		GitRef:          "base123..abcdef1234567890",
		CIBaseBranch:    "main",
		Agent:           "codex",
		ReviewType:      "security",
		Status:          storage.JobStatusFailed,
		Error:           review.QuotaErrorPrefix + "agent codex quota cooldown active",
		RetryCount:      2,
		PanelRole:       storage.PanelRoleMember,
		PanelName:       "ci",
		PanelMemberName: "security-codex",
	}

	payload := buildDiscordCIJobFailedPayload(Event{}, job)

	if assert.Len(t, payload.Embeds, 1) {
		embed := payload.Embeds[0]
		assert.Equal(t, "roborev CI job failed", embed.Title)
		fields := discordEmbedFieldsByName(embed.Fields)
		assert.Equal(t, "api", fields["Repository"])
		assert.Contains(t, fields["Job"], "42")
		assert.Contains(t, fields["Job"], "member")
		assert.Contains(t, fields["Job"], "security-codex")
		assert.Equal(t, "codex", fields["Agent"])
		assert.Equal(t, "main", fields["Branch"])
		assert.Equal(t, "quota/cooldown", fields["Failure"])
		assert.Equal(t, "2", fields["Retry count"])
		assert.Contains(t, fields["Error"], "quota cooldown active")
		assert.Contains(t, fields["Ref"], "abcdef1")
	}
}

func discordEmbedFieldsByName(fields []discordEmbedField) map[string]string {
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[f.Name] = f.Value
	}
	return out
}

func TestPostDiscordWebhookPostsJSON(t *testing.T) {
	type request struct {
		contentType string
		payload     discordWebhookPayload
	}
	reqCh := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload discordWebhookPayload
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		reqCh <- request{contentType: r.Header.Get("Content-Type"), payload: payload}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	payload := discordWebhookPayload{Embeds: []discordEmbed{{Title: "roborev CI job failed"}}}

	var logs []string
	ok := postDiscordWebhook(context.Background(), server.URL, payload, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	assert.True(t, ok)
	assert.Empty(t, logs)
	var got request
	require.Eventually(t, func() bool {
		select {
		case got = <-reqCh:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "application/json", got.contentType)
	require.Len(t, got.payload.Embeds, 1)
	assert.Equal(t, "roborev CI job failed", got.payload.Embeds[0].Title)
}

func TestPostDiscordWebhookRedactsURLInLogs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	webhookURL, err := neturl.Parse(server.URL)
	require.NoError(t, err)
	webhookURL.User = neturl.UserPassword("token", "secret")
	webhookURL.Path = "/api/webhooks/123456/sensitive-token"
	webhookURL.RawQuery = "api_key=12345"
	webhookURL.Fragment = "frag"

	var logs []string
	ok := postDiscordWebhook(context.Background(), webhookURL.String(), discordWebhookPayload{}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	assert.False(t, ok)
	require.NotEmpty(t, logs)
	logOutput := strings.Join(logs, "\n")
	assert.Contains(t, logOutput, "502 Bad Gateway")
	assert.Contains(t, logOutput, "/...")
	assert.NotContains(t, logOutput, "token")
	assert.NotContains(t, logOutput, "secret")
	assert.NotContains(t, logOutput, "api_key")
	assert.NotContains(t, logOutput, "12345")
	assert.NotContains(t, logOutput, "frag")
	assert.NotContains(t, logOutput, "sensitive-token")
}
