package mcpserver

import (
	"context"
	"strings"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

// KataBackend extends the review backend without making Kata mandatory for
// transports that do not have a configured connection.
type KataBackend interface {
	KataIssues(context.Context, KataReadInput) (KataIssuesResult, error)
	KataContext(context.Context, KataReadInput, string) (katacontext.KataReplyContext, error)
	KataReply(context.Context, kataresponse.Request) (storage.KataResponseStatus, error)
}

type KataReadInput struct {
	JobID      int64  `json:"job_id,omitempty"`
	ReviewUUID string `json:"review_uuid,omitempty" jsonschema:"immutable UUID from the displayed review"`
	Ref        string `json:"ref,omitempty" jsonschema:"frozen qualified issue reference or full issue UID; context only"`
}

type KataFindingView struct {
	ID      string `json:"id"`
	Issue   string `json:"issue,omitempty"`
	Comment string `json:"comment,omitempty"`
	State   string `json:"state"`
}

type KataPublicationView struct {
	ReviewUUID  string            `json:"review_uuid,omitempty"`
	OperationID string            `json:"operation_id"`
	Issue       string            `json:"issue,omitempty"`
	IssueUID    string            `json:"issue_uid,omitempty"`
	State       string            `json:"state"`
	Policy      string            `json:"policy"`
	Findings    []KataFindingView `json:"findings"`
}

type KataIssuesResult struct {
	JobID        int64                 `json:"job_id"`
	Publications []KataPublicationView `json:"publications"`
}

func (s *Server) registerKataTools() {
	for _, tool := range []struct{ name, description string }{
		{"roborev_kata_issues", "Look up a review's frozen Kata publications and finding root mappings. Pass review_uuid from the displayed review to avoid newer reruns."},
		{"roborev_kata_context", "Read pinned findings and attributed, untrusted reply claims. Records the snapshot consumed by this tool call. No claim establishes resolution; unavailable targets and truncation remain explicit."},
	} {
		definition := &mcp.Tool{Name: tool.name, Description: tool.description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
		if tool.name == "roborev_kata_issues" {
			mcp.AddTool(s.mcp, definition, wrapTool(s.kataIssues))
		} else {
			mcp.AddTool(s.mcp, definition, wrapTool(s.kataContext))
		}
	}
	if s.allowKataWrites {
		mcp.AddTool(s.mcp, &mcp.Tool{Name: "roborev_kata_reply", Description: "Enqueue a durable typed response to a mapped finding. operation_id is an opaque, case-sensitive idempotency key; reuse it with the identical payload on retry. A fix is a reply after commit, never a confirm. Pending or duplicate replies remain unresolved. Does not close reviews."}, wrapTool(s.kataReply))
	}
}

func (s *Server) kataBackend() (KataBackend, error) {
	backend, ok := s.backend.(KataBackend)
	if !ok {
		return nil, NewError(ErrorCodeUnavailable, "Kata context is unavailable through this backend")
	}
	return backend, nil
}

func (s *Server) kataIssues(ctx context.Context, in KataReadInput) (KataIssuesResult, error) {
	if in.JobID <= 0 || strings.TrimSpace(in.Ref) != "" {
		return KataIssuesResult{}, NewError(ErrorCodeInvalidArgument, "positive job_id is required")
	}
	b, err := s.kataBackend()
	if err != nil {
		return KataIssuesResult{}, err
	}
	return b.KataIssues(ctx, in)
}

func (s *Server) kataContext(ctx context.Context, in KataReadInput) (katacontext.KataReplyContext, error) {
	if (in.JobID > 0) == (strings.TrimSpace(in.Ref) != "") || in.JobID < 0 {
		return katacontext.KataReplyContext{}, NewError(ErrorCodeInvalidArgument, "select positive job_id or frozen issue ref")
	}
	b, err := s.kataBackend()
	if err != nil {
		return katacontext.KataReplyContext{}, err
	}
	return b.KataContext(ctx, in, uuid.New().String()) //nolint:forbidigo // Consumption UUID in the daemon API request.
}

func (s *Server) kataReply(ctx context.Context, in kataresponse.Request) (storage.KataResponseStatus, error) {
	if !s.allowKataWrites {
		return storage.KataResponseStatus{}, NewError(ErrorCodeUnavailable, "Kata writes require explicit allow-kata-writes")
	}
	if err := in.Validate(); err != nil {
		return storage.KataResponseStatus{}, NewError(ErrorCodeInvalidArgument, err.Error())
	}
	b, err := s.kataBackend()
	if err != nil {
		return storage.KataResponseStatus{}, err
	}
	return b.KataReply(ctx, in)
}
