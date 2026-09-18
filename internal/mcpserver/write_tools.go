package mcpserver

import (
	"context"
	"strings"
	"time"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type AddCommentInput struct {
	JobID     int64  `json:"job_id" jsonschema:"Review job ID to comment on"`
	Commenter string `json:"commenter" jsonschema:"Name of the person or agent recording the comment"`
	Comment   string `json:"comment" jsonschema:"Comment text"`
}

type closeReviewInput struct {
	JobID int64 `json:"job_id" jsonschema:"Review job ID to close"`
}

type SnoozeInput struct {
	RepoPath     string    `json:"repo_path" jsonschema:"Tracked repository root_path"`
	WorktreePath string    `json:"worktree_path" jsonschema:"Current worktree root"`
	Branch       string    `json:"branch,omitempty" jsonschema:"Current branch"`
	Enabled      bool      `json:"enabled" jsonschema:"True to snooze hooks; false to resume them"`
	SnoozedUntil time.Time `json:"snoozed_until,omitempty" jsonschema:"Snooze expiry as an RFC3339 timestamp; required when enabled"`
}

type SnoozeOutput struct {
	Snoozed      bool       `json:"snoozed"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
}

type completeFixInput struct {
	FixSessionID uuid.UUID `json:"fix_session_id" jsonschema:"Exact fix-session UUID supplied by the hook"`
}

type successOutput struct {
	Success bool `json:"success"`
}

func (s *Server) registerWriteTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "roborev_add_comment", Description: "Record a comment on an existing review job. Use the synthesis parent for panel reviews."}, wrapTool(s.addComment))
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "roborev_close_review", Description: "Close an existing review after its findings have been resolved or shown invalid. Use the synthesis parent for panel reviews."}, wrapTool(s.closeReview))
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "roborev_snooze", Description: "Snooze or resume Agent Hook reminders for a repository, worktree, and branch. Review generation continues."}, wrapTool(s.snooze))
	mcp.AddTool(s.mcp, &mcp.Tool{Name: "roborev_complete_fix", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"fix_session_id": map[string]any{"type": "string", "format": "uuid"}}, "required": []string{"fix_session_id"}}, Description: "Complete the exact Agent Hook fix session supplied in the current hook instruction after auditing its original reviews. Do not invent or discover a session ID."}, wrapTool(s.completeFix))
}

func (s *Server) addComment(ctx context.Context, in AddCommentInput) (commentRow, error) {
	if in.JobID <= 0 || strings.TrimSpace(in.Commenter) == "" || strings.TrimSpace(in.Comment) == "" {
		return commentRow{}, NewError(ErrorCodeInvalidArgument, "positive job_id, commenter, and comment are required")
	}
	out, err := s.backend.AddComment(ctx, in)
	if err != nil {
		return commentRow{}, err
	}
	return commentRow{ID: out.ID, Responder: out.Responder, Response: out.Response, Source: out.Source, CreatedAt: out.CreatedAt}, nil
}

func (s *Server) closeReview(ctx context.Context, in closeReviewInput) (successOutput, error) {
	if in.JobID <= 0 {
		return successOutput{}, NewError(ErrorCodeInvalidArgument, "positive job_id is required")
	}
	err := s.backend.CloseReview(ctx, in.JobID)
	return successOutput{Success: err == nil}, err
}

func (s *Server) snooze(ctx context.Context, in SnoozeInput) (SnoozeOutput, error) {
	if strings.TrimSpace(in.RepoPath) == "" || strings.TrimSpace(in.WorktreePath) == "" {
		return SnoozeOutput{}, NewError(ErrorCodeInvalidArgument, "repo_path and worktree_path are required")
	}
	if in.Enabled && !in.SnoozedUntil.After(time.Now()) {
		return SnoozeOutput{}, NewError(ErrorCodeInvalidArgument, "snoozed_until must be in the future")
	}
	return s.backend.Snooze(ctx, in)
}

func (s *Server) completeFix(ctx context.Context, in completeFixInput) (successOutput, error) {
	if in.FixSessionID == uuid.Nil() {
		return successOutput{}, NewError(ErrorCodeInvalidArgument, "fix_session_id is required")
	}
	err := s.backend.CompleteFix(ctx, in.FixSessionID)
	return successOutput{Success: err == nil}, err
}
