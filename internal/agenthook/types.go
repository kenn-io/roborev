package agenthook

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const ServiceName = "roborev-agent-hook"

// Input is the normalized hook payload used by roborev's agent-hook daemon.
// Claude/Codex send snake_case keys; Grok Build sends camelCase (see
// xai-org/grok-build HookEventEnvelope). DecodeInput accepts both.
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

// DecodeInput reads a harness hook payload and normalizes Claude snake_case and
// Grok camelCase envelopes into Input. Hook event names are canonicalized to
// Claude-style PascalCase (PreToolUse, PostToolUse, Stop) so StateStore.Record
// can share one switch.
func DecodeInput(r io.Reader) (Input, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return Input{}, err
	}
	return inputFromRaw(raw)
}

func inputFromRaw(raw map[string]json.RawMessage) (Input, error) {
	var in Input
	in.SessionID = firstString(raw, "session_id", "sessionId")
	in.TranscriptPath = firstString(raw, "transcript_path", "transcriptPath")
	in.CWD = firstString(raw, "cwd")
	in.HookEventName = NormalizeHookEventName(firstString(raw, "hook_event_name", "hookEventName"))
	in.TurnID = firstString(raw, "turn_id", "turnId", "prompt_id", "promptId")
	in.StopHookActive = firstBool(raw, "stop_hook_active", "stopHookActive")
	in.LastAssistant = firstString(raw, "last_assistant_message", "lastAssistantMessage")
	in.ToolName = firstString(raw, "tool_name", "toolName")
	in.ToolUseID = firstString(raw, "tool_use_id", "toolUseId")
	if toolInput, ok := firstRaw(raw, "tool_input", "toolInput"); ok {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(toolInput, &m); err != nil {
			return Input{}, fmt.Errorf("decode tool_input: %w", err)
		}
		in.ToolInput = m
	}
	if resp, ok := firstRaw(raw, "tool_response", "toolResponse", "tool_result", "toolResult"); ok {
		in.ToolResponse = resp
	}
	return in, nil
}

// NormalizeHookEventName maps harness event spellings onto the PascalCase names
// StateStore.Record expects. Grok serializes display names like pre_tool_use /
// stop; Claude sends PreToolUse / Stop.
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
		// Preserve already-canonical PascalCase and unknown events (skipped).
		return strings.TrimSpace(name)
	}
}

func firstString(raw map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		if s != "" {
			return s
		}
	}
	return ""
}

func firstBool(raw map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			continue
		}
		// First successfully decoded key wins — including explicit false.
		// Do not skip false and fall through to later aliases.
		return b
	}
	return false
}

func firstRaw(raw map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok {
			continue
		}
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		return v, true
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
	RoborevServerAddr     string `json:"roborev_server_addr,omitempty"`
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

type SessionState struct {
	Count                       int                 `json:"count"`
	StopCountsSincePrompt       map[string]int      `json:"stop_counts_since_prompt,omitempty"`
	CommitCount                 int                 `json:"commit_count,omitempty"`
	CommitCountsSincePrompt     map[string]int      `json:"commit_counts_since_prompt,omitempty"`
	CommitSHAsSincePrompt       map[string][]string `json:"commit_shas_since_prompt,omitempty"`
	FailedReviewCount           int                 `json:"failed_review_count,omitempty"`
	FailedReviewTriggeredCounts map[string]int      `json:"failed_review_triggered_counts,omitempty"`
	ReminderPromptCount         int                 `json:"remind_count,omitempty"`
	LastTurnID                  string              `json:"last_turn_id,omitempty"`
	LastCWD                     string              `json:"last_cwd,omitempty"`
	LastCommitRepo              string              `json:"last_commit_repo,omitempty"`
	LastCommitHead              string              `json:"last_commit_head,omitempty"`
	LastFailedReviewRepo        string              `json:"last_failed_review_repo,omitempty"`
	LastFailedReviewBranch      string              `json:"last_failed_review_branch,omitempty"`
	RepoHeads                   map[string]string   `json:"repo_heads,omitempty"`
	WorktreeLineageKeys         map[string]string   `json:"worktree_lineage_keys,omitempty"`
	LastSeenAt                  time.Time           `json:"last_seen_at,omitzero"`
	TriggeredAt                 time.Time           `json:"triggered_at,omitzero"`
	CommitTriggeredAt           time.Time           `json:"commit_triggered_at,omitzero"`
	FailedReviewTriggeredAt     time.Time           `json:"failed_review_triggered_at,omitzero"`
}

type Snapshot struct {
	Sessions map[string]SessionState `json:"sessions"`
}

type StateStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]SessionState
}

type ResetOptions struct {
	All bool
}
