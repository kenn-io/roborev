// Package mcpserver exposes read-only roborev review data to Model Context
// Protocol clients over stdio or streamable HTTP.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

// Backend is the read-only roborev data boundary used by MCP tools. The
// daemon implements it in-process; the CLI implements it over the daemon
// HTTP API.
type Backend interface {
	Status(context.Context) (*storage.DaemonStatus, error)
	ListRepos(context.Context, ReposQuery) ([]storage.RepoWithCount, error)
	ListBranches(ctx context.Context, repoPath string) ([]storage.BranchWithCount, error)
	ListJobs(context.Context, JobsQuery) (JobsPage, error)
	GetReview(context.Context, ReviewRef) (*storage.Review, error)
	ListComments(context.Context, CommentRef) ([]storage.Response, error)
	GetJobOutput(ctx context.Context, jobID int64) (JobOutput, error)
}

// ReposQuery filters tracked repositories.
type ReposQuery struct {
	Prefix string
	Branch string
}

// JobsQuery filters the review job listing. Zero values mean "no filter".
// A positive ID returns that single job and ignores the other filters.
type JobsQuery struct {
	ID       int64
	GitRef   string
	RepoPath string
	Branch   string
	Status   string
	JobType  string
	Closed   *bool
	Limit    int
	Cursor   string
}

// JobsPage is one page of review jobs.
type JobsPage struct {
	Jobs       []storage.ReviewJob
	HasMore    bool
	NextCursor string
}

// ReviewRef identifies a review by job ID or by commit SHA. A positive JobID
// takes precedence.
type ReviewRef struct {
	JobID int64
	SHA   string
}

// CommentRef identifies a comment thread by job ID, legacy commit ID, or
// commit SHA. A positive JobID takes precedence, then CommitID, then SHA.
type CommentRef struct {
	JobID    int64
	CommitID int64
	SHA      string
}

// OutputLine mirrors the daemon's streamed output line format.
type OutputLine struct {
	Timestamp time.Time `json:"ts"`
	Text      string    `json:"text"`
	Type      string    `json:"line_type"`
}

// JobOutput is the accumulated output of a job.
type JobOutput struct {
	JobID   int64        `json:"job_id"`
	Status  string       `json:"status"`
	Lines   []OutputLine `json:"lines"`
	HasMore bool         `json:"has_more"`
}

// Error codes reported to MCP clients.
const (
	ErrorCodeNotFound        = "not_found"
	ErrorCodeInvalidArgument = "invalid_argument"
	ErrorCodeUnavailable     = "unavailable"
	ErrorCodeInternal        = "internal"
)

// Error is a backend failure with a stable code that tools surface to clients.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError builds a backend error.
func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func errorCode(err error) string {
	if backendErr, ok := errors.AsType[*Error](err); ok {
		return backendErr.Code
	}
	return ErrorCodeInternal
}
