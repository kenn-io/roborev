package daemon

import (
	"context"
	"encoding/json/v2"

	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/storage"
)

func (b mcpBackend) KataIssues(ctx context.Context, in mcpserver.KataReadInput) (mcpserver.KataIssuesResult, error) {
	out, err := b.server.humaKataPublications(ctx, &KataPublicationsInput{JobID: in.JobID, ReviewUUID: in.ReviewUUID})
	if err != nil {
		return mcpserver.KataIssuesResult{}, mcpError(err)
	}
	// The MCP view exposes the same safe identifiers, never the local handoff.
	raw, err := json.Marshal(out.Body)
	if err != nil {
		return mcpserver.KataIssuesResult{}, err
	}
	var view mcpserver.KataIssuesResult
	if err := json.Unmarshal(raw, &view); err != nil {
		return view, err
	}
	return view, nil
}

func (b mcpBackend) KataContext(ctx context.Context, in mcpserver.KataReadInput, runID string) (katacontext.KataReplyContext, error) {
	out, err := b.server.humaKataConsumeContext(ctx, &KataConsumeContextInput{Body: KataConsumeContextRequest{
		JobID: in.JobID, ReviewUUID: in.ReviewUUID, Ref: in.Ref, RunID: runID, Purpose: "mcp",
	}})
	if err != nil {
		return katacontext.KataReplyContext{}, mcpError(err)
	}
	return out.Body, nil
}

func (b mcpBackend) KataReply(ctx context.Context, in kataresponse.Request) (storage.KataResponseStatus, error) {
	out, err := b.server.humaKataResponse(ctx, &KataResponseInput{Body: KataResponseRequest(in)})
	if err != nil {
		return storage.KataResponseStatus{}, mcpError(err)
	}
	return out.Body, nil
}
