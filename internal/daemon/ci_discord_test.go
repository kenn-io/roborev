package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
