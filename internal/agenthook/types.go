package agenthook

import (
	"encoding/json"
	"sync"
	"time"
)

const ServiceName = "roborev-agent-hook"

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
	Count                       int               `json:"count"`
	StopCountSincePrompt        int               `json:"stop_count_since_prompt,omitempty"`
	CommitCount                 int               `json:"commit_count,omitempty"`
	CommitCountsSincePrompt     map[string]int    `json:"commit_counts_since_prompt,omitempty"`
	FailedReviewCount           int               `json:"failed_review_count,omitempty"`
	FailedReviewTriggeredCounts map[string]int    `json:"failed_review_triggered_counts,omitempty"`
	ReminderPromptCount         int               `json:"remind_count,omitempty"`
	LastTurnID                  string            `json:"last_turn_id,omitempty"`
	LastCWD                     string            `json:"last_cwd,omitempty"`
	LastCommitRepo              string            `json:"last_commit_repo,omitempty"`
	LastCommitHead              string            `json:"last_commit_head,omitempty"`
	LastFailedReviewRepo        string            `json:"last_failed_review_repo,omitempty"`
	LastFailedReviewBranch      string            `json:"last_failed_review_branch,omitempty"`
	RepoHeads                   map[string]string `json:"repo_heads,omitempty"`
	LastSeenAt                  time.Time         `json:"last_seen_at,omitzero"`
	TriggeredAt                 time.Time         `json:"triggered_at,omitzero"`
	CommitTriggeredAt           time.Time         `json:"commit_triggered_at,omitzero"`
	FailedReviewTriggeredAt     time.Time         `json:"failed_review_triggered_at,omitzero"`
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
