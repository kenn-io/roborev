package agenthook

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Input struct {
	SessionID      string                     `json:"session_id"`
	TranscriptPath string                     `json:"transcript_path,omitempty"`
	CWD            string                     `json:"cwd,omitempty"`
	HookEventName  string                     `json:"hook_event_name,omitempty"`
	TurnID         string                     `json:"turn_id,omitempty"`
	StopHookActive bool                       `json:"stop_hook_active,omitempty"`
	LastAssistant  string                     `json:"last_assistant_message,omitempty"`
	ToolName       string                     `json:"tool_name,omitempty"`
	ToolUseID      string                     `json:"tool_use_id,omitempty"`
	ToolInput      map[string]json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   json.RawMessage            `json:"tool_response,omitempty"`
}

// DecodeInput normalizes Claude-style snake_case and Grok Build camelCase
// hook envelopes for the one profile that kit does not yet expose.
func DecodeInput(r io.Reader) (Input, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return Input{}, err
	}
	var input Input
	input.SessionID = firstString(raw, "session_id", "sessionId")
	input.TranscriptPath = firstString(raw, "transcript_path", "transcriptPath")
	input.CWD = firstString(raw, "cwd")
	input.HookEventName = NormalizeHookEventName(firstString(raw, "hook_event_name", "hookEventName"))
	input.TurnID = firstString(raw, "turn_id", "turnId", "prompt_id", "promptId")
	input.StopHookActive = firstBool(raw, "stop_hook_active", "stopHookActive")
	input.LastAssistant = firstString(raw, "last_assistant_message", "lastAssistantMessage")
	input.ToolName = firstString(raw, "tool_name", "toolName")
	input.ToolUseID = firstString(raw, "tool_use_id", "toolUseId")
	if toolInput, ok := firstRaw(raw, "tool_input", "toolInput"); ok {
		if err := json.Unmarshal(toolInput, &input.ToolInput); err != nil {
			return Input{}, fmt.Errorf("decode tool_input: %w", err)
		}
	}
	if response, ok := firstRaw(raw, "tool_response", "toolResponse", "tool_result", "toolResult"); ok {
		input.ToolResponse = response
	}
	return input, nil
}

func NormalizeHookEventName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "pretooluse", "pre_tool_use":
		return "PreToolUse"
	case "posttooluse", "post_tool_use":
		return "PostToolUse"
	case "stop":
		return "Stop"
	case "":
		return ""
	default:
		return strings.TrimSpace(name)
	}
}

func firstString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		var decoded string
		if err := json.Unmarshal(value, &decoded); err == nil && decoded != "" {
			return decoded
		}
	}
	return ""
}

func firstBool(raw map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		var decoded bool
		if err := json.Unmarshal(value, &decoded); err == nil {
			return decoded
		}
	}
	return false
}

func firstRaw(raw map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		value, ok := raw[key]
		if ok && len(value) > 0 && string(value) != "null" {
			return value, true
		}
	}
	return nil, false
}

func (i Input) Command() string {
	if i.ToolInput == nil {
		return ""
	}
	raw, ok := i.ToolInput["command"]
	if !ok {
		return ""
	}
	var command string
	if err := json.Unmarshal(raw, &command); err != nil {
		return ""
	}
	return command
}

type Request struct {
	Event                 Input  `json:"event"`
	Threshold             int    `json:"threshold"`
	CommitThreshold       int    `json:"commit_threshold"`
	FailedReviewThreshold int    `json:"failed_review_threshold"`
	Instruction           string `json:"instruction"`
	DeferPostToolReminder bool   `json:"defer_post_tool_reminder,omitempty"`
}

type Response struct {
	SessionID             string `json:"session_id"`
	Count                 int    `json:"count"`
	Threshold             int    `json:"threshold"`
	CommitCount           int    `json:"commit_count,omitempty"`
	CommitThreshold       int    `json:"commit_threshold,omitempty"`
	FailedReviewCount     int    `json:"failed_review_count,omitempty"`
	FailedReviewThreshold int    `json:"failed_review_threshold,omitempty"`
	ReminderPromptCount   int    `json:"remind_count,omitempty"`
	Triggered             bool   `json:"triggered"`
	TriggeredBy           string `json:"triggered_by,omitempty"`
	Reason                string `json:"reason,omitempty"`
	Skipped               bool   `json:"skipped,omitempty"`
}

type reviewIDSet map[int64]struct{}

type SessionState struct {
	Count                       int                        `json:"count"`
	StopCountsSincePrompt       map[string]int             `json:"stop_counts_since_prompt,omitempty"`
	CommitCount                 int                        `json:"commit_count,omitempty"`
	CommitCountsSincePrompt     map[string]int             `json:"commit_counts_since_prompt,omitempty"`
	CommitSHAsSincePrompt       map[string][]string        `json:"commit_shas_since_prompt,omitempty"`
	FailedReviewCount           int                        `json:"failed_review_count,omitempty"`
	FailedReviewTriggeredCounts map[string]int             `json:"failed_review_triggered_counts,omitempty"`
	AcknowledgedReviewIDs       map[string]reviewIDSet     `json:"acknowledged_review_ids,omitempty"`
	ReminderPromptCount         int                        `json:"remind_count,omitempty"`
	LastTurnID                  string                     `json:"last_turn_id,omitempty"`
	LastCWD                     string                     `json:"last_cwd,omitempty"`
	LastCommitRepo              string                     `json:"last_commit_repo,omitempty"`
	LastCommitHead              string                     `json:"last_commit_head,omitempty"`
	LastFailedReviewRepo        string                     `json:"last_failed_review_repo,omitempty"`
	LastFailedReviewBranch      string                     `json:"last_failed_review_branch,omitempty"`
	RepoHeads                   map[string]string          `json:"repo_heads,omitempty"`
	WorktreeLineageKeys         map[string]string          `json:"worktree_lineage_keys,omitempty"`
	LastSeenAt                  time.Time                  `json:"last_seen_at,omitzero"`
	TriggeredAt                 time.Time                  `json:"triggered_at,omitzero"`
	CommitTriggeredAt           time.Time                  `json:"commit_triggered_at,omitzero"`
	FailedReviewTriggeredAt     time.Time                  `json:"failed_review_triggered_at,omitzero"`
	PendingReminders            map[string]PendingReminder `json:"pending_reminders,omitempty"`
}

func (s *SessionState) UnmarshalJSON(body []byte) error {
	type persistedSessionState SessionState
	var current persistedSessionState
	if err := json.Unmarshal(body, &current); err != nil {
		return err
	}
	// Releases before v0.64 stored one session-wide Stop counter. Carry it to
	// the only identifiable recent lineage, then let the next save remove the
	// old field. Ambiguous multi-workspace state safely resets. Remove this
	// migration after v0.66 ships. See #1012.
	var legacy struct {
		StopCountSincePrompt int `json:"stop_count_since_prompt,omitempty"`
	}
	if err := json.Unmarshal(body, &legacy); err != nil {
		return err
	}
	state := SessionState(current)
	if legacy.StopCountSincePrompt > 0 && len(state.StopCountsSincePrompt) == 0 {
		if key := legacyStopCountLineage(state); key != "" {
			state.StopCountsSincePrompt = map[string]int{key: legacy.StopCountSincePrompt}
		}
	}
	*s = state
	return nil
}

func legacyStopCountLineage(state SessionState) string {
	if len(state.WorktreeLineageKeys) > 1 {
		return ""
	}
	if len(state.WorktreeLineageKeys) == 1 {
		for _, lineage := range state.WorktreeLineageKeys {
			return lineage
		}
	}
	if len(state.RepoHeads) > 1 {
		return ""
	}
	if len(state.RepoHeads) == 1 {
		for key := range state.RepoHeads {
			return key
		}
	}
	if state.LastFailedReviewRepo != "" {
		return repoHeadKey(state.LastFailedReviewRepo, state.LastFailedReviewBranch)
	}
	return ""
}

type PendingReminder struct {
	TriggeredBy         string    `json:"triggered_by"`
	Reason              string    `json:"reason"`
	Instruction         string    `json:"instruction,omitempty"`
	TrackedRepoRoot     string    `json:"tracked_repo_root"`
	TrackedRepoIdentity string    `json:"tracked_repo_identity,omitempty"`
	WorktreeRoot        string    `json:"worktree_root"`
	Branch              string    `json:"branch,omitempty"`
	Head                string    `json:"head,omitempty"`
	LineageKey          string    `json:"lineage_key"`
	CommitCount         int       `json:"commit_count,omitempty"`
	FailedReviewCount   int       `json:"failed_review_count,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

type Snapshot struct {
	Sessions map[string]SessionState `json:"sessions"`
}

type StateStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]SessionState
	reviews  ReviewSource
}

type ResetOptions struct {
	All bool
}
