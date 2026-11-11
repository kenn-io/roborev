package daemon

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/kata"
)

// KataResponseRequest freezes the selected destination and comment for a reply.
type KataResponseRequest struct {
	Project        string `json:"project"`
	DestinationID  string `json:"destination_id"`
	Generation     uint64 `json:"generation"`
	IssueUID       string `json:"issue_uid"`
	TargetUID      string `json:"target_uid"`
	Kind           string `json:"kind"`
	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
}

type KataResponseInput struct{ Body KataResponseRequest }

type KataReplyReceipt struct {
	UID      string `json:"uid"`
	Author   string `json:"author"`
	Teammate string `json:"teammate"`
	Changed  bool   `json:"changed"`
}

type KataResponseOutput struct{ Body KataReplyReceipt }

func (s *Server) humaKataResponse(ctx context.Context, in *KataResponseInput) (*KataResponseOutput, error) {
	req := in.Body
	if req.Project == "" || req.DestinationID == "" || req.IssueUID == "" || req.TargetUID == "" {
		return nil, huma.Error400BadRequest("missing destination or target")
	}
	if err := kata.ValidateReply(req.Kind, req.Body, req.IdempotencyKey); err != nil {
		return nil, kataProxyError(err)
	}
	cfg, generation := s.configWatcher.kataSnapshot()
	if generation != req.Generation {
		return nil, huma.Error409Conflict("Kata configuration changed; read the destination again")
	}
	client, err := kataProxyClientFor(cfg, nil)
	if err != nil {
		return nil, err
	}
	project, err := client.ResolvePublicationProject(ctx, req.Project)
	if err != nil {
		return nil, kataProxyError(err)
	}
	if project.Name != req.Project || kataDestinationID(client.PublicationEndpoint(), project.UID) != req.DestinationID {
		return nil, huma.Error409Conflict("Kata destination changed")
	}
	issue, err := client.ShowPublicationIssue(ctx, project, req.IssueUID)
	if err != nil {
		return nil, kataProxyError(err)
	}
	target, err := kata.ResolvePublicationTarget(project, issue, req.TargetUID)
	if err != nil {
		return nil, kataProxyError(err)
	}
	if target != req.TargetUID {
		return nil, huma.Error409Conflict("target UID changed")
	}
	// Keep reload from changing the credential or destination during the bounded write.
	cw := s.configWatcher
	cw.cfgMu.RLock()
	defer cw.cfgMu.RUnlock()
	if cw.reloadCounter != generation || !cw.cfg.Integrations.Kata.Enabled || cw.cfg.Integrations.Kata != cfg {
		return nil, huma.Error409Conflict("Kata configuration changed; read the destination again")
	}
	writeCtx, cancel := context.WithTimeout(ctx, kata.DefaultRequestTimeout)
	defer cancel()
	receipt, err := client.Comment(writeCtx, project, req.IssueUID, target, req.Kind, req.Body, req.IdempotencyKey, cfg.ResolvedTeammate())
	if err != nil {
		return nil, kataProxyError(err)
	}
	if receipt.Comment.Teammate == nil {
		return nil, huma.Error502BadGateway("invalid Kata comment receipt")
	}
	return &KataResponseOutput{Body: KataReplyReceipt{UID: receipt.Comment.UID, Author: receipt.Comment.Author, Teammate: *receipt.Comment.Teammate, Changed: receipt.Changed}}, nil
}
