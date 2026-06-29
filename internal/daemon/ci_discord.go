package daemon

import (
	"fmt"
	"strconv"
	"strings"

	gitpkg "go.kenn.io/roborev/internal/git"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
)

const (
	discordFailureQuotaCooldown = "quota/cooldown"
	discordFailureOutage        = "provider/session outage"
	discordFailureTimeout       = "timeout"
	discordFailureError         = "error"
	discordFieldLimit           = 900
)

type discordWebhookPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title  string              `json:"title,omitempty"`
	Color  int                 `json:"color,omitempty"`
	Fields []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

func buildDiscordCIJobFailedPayload(event Event, job storage.ReviewJob) discordWebhookPayload {
	agentName := firstNonEmpty(event.Agent, job.Agent)
	errorText := firstNonEmpty(job.Error, event.Error)

	fields := []discordEmbedField{
		{Name: "Repository", Value: nonEmpty(firstNonEmpty(event.RepoName, job.RepoName), "unknown"), Inline: true},
		{Name: "Job", Value: discordJobSummary(job), Inline: true},
		{Name: "Agent", Value: nonEmpty(agentName, "unknown"), Inline: true},
		{Name: "Ref", Value: discordRefSummary(job, event), Inline: true},
		{Name: "Branch", Value: nonEmpty(job.HookBranch(), "unknown"), Inline: true},
		{Name: "Failure", Value: discordFailureClass(job, event.Error), Inline: true},
		{Name: "Retry count", Value: strconv.Itoa(job.RetryCount), Inline: true},
		{Name: "Error", Value: trimDiscordField(errorText), Inline: false},
	}

	return discordWebhookPayload{
		Embeds: []discordEmbed{{
			Title:  "roborev CI job failed",
			Color:  0xD73A49,
			Fields: fields,
		}},
	}
}

func discordFailureClass(job storage.ReviewJob, eventError string) string {
	errorText := firstNonEmpty(job.Error, eventError)
	result := reviewpkg.ReviewResult{
		Status: string(job.Status),
		Error:  errorText,
	}
	if reviewpkg.IsQuotaFailure(result) {
		return discordFailureQuotaCooldown
	}
	if reviewpkg.IsTransientFailure(result) {
		return discordFailureOutage
	}
	if strings.Contains(errorText, agentTimeoutErrorPrefix) {
		return discordFailureTimeout
	}
	if reviewpkg.IsGenuineFailure(result) {
		return discordFailureError
	}
	return discordFailureError
}

func discordJobSummary(job storage.ReviewJob) string {
	parts := []string{fmt.Sprintf("#%d", job.ID)}
	for _, p := range []string{job.PanelRole, job.PanelName, job.PanelMemberName, job.ReviewType} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " / ")
}

func discordRefSummary(job storage.ReviewJob, event Event) string {
	ref := firstNonEmpty(event.SHA, job.GitRef)
	if ref == "" {
		return "unknown"
	}
	ref = headOf(ref)
	return gitpkg.ShortSHA(ref)
}

func trimDiscordField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if len(s) <= discordFieldLimit {
		return s
	}
	return reviewpkg.TrimPartialRune(s[:discordFieldLimit-3]) + "..."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
