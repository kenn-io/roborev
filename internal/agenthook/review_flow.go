package agenthook

import (
	"context"
	"strings"

	"go.kenn.io/roborev/internal/agentguide"
	"go.kenn.io/roborev/internal/config"
)

// ReviewFlowSource supplies a repository's trusted review flow.
type ReviewFlowSource interface {
	ReviewFlow(context.Context, string) config.AgentHookReviewFlow
}

func (s *StateStore) reviewFlow(ctx context.Context, repoRoot string) config.AgentHookReviewFlow {
	if strings.TrimSpace(repoRoot) == "" || ctx.Err() != nil {
		return config.AgentHookReviewFlow{}
	}
	if s.flows != nil {
		return s.flows.ReviewFlow(ctx, repoRoot)
	}
	flow, _ := config.LoadAgentHookReviewFlow(ctx, repoRoot)
	return flow
}

func withReviewFlow(reason string, flow config.AgentHookReviewFlow) string {
	if strings.TrimSpace(flow.Text) == "" {
		return reason
	}
	source := "in the working tree"
	if flow.Ref != "" {
		source = "on " + quotedLabel(flow.Ref)
	}
	return reason + "\n\nRepository review flow, from .roborev.toml " + source + ". It refines the Which disposition step above for this repository:\n\n" + agentguide.FenceDOT(flow.Text)
}
