package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/agenthook"
)

type roborevAgentHookHandler struct {
	kitagenthook.NoopHandler
	agent  kitagenthook.Agent
	opts   agenthook.Options
	stderr io.Writer
}

func newRoborevAgentHookHandler(
	agent kitagenthook.Agent,
	opts agenthook.Options,
	stderr io.Writer,
) roborevAgentHookHandler {
	return roborevAgentHookHandler{agent: agent, opts: opts, stderr: stderr}
}

func (h roborevAgentHookHandler) request(
	common kitagenthook.CommonInput,
	toolName string,
	toolInput json.RawMessage,
) (agenthook.Request, error) {
	input := agenthook.Input{
		SessionID:      common.SessionID,
		TranscriptPath: common.TranscriptPath,
		CWD:            common.CWD,
		HookEventName:  string(common.HookEventName),
		TurnID:         common.TurnID,
		ToolName:       toolName,
	}
	if len(toolInput) > 0 {
		if err := json.Unmarshal(toolInput, &input.ToolInput); err != nil {
			return agenthook.Request{}, fmt.Errorf("decode normalized tool input: %w", err)
		}
	}
	return agenthook.Request{
		Event:                 input,
		Threshold:             h.opts.TurnThreshold,
		CommitThreshold:       h.opts.CommitThreshold,
		FailedReviewThreshold: h.opts.FailedReviewThreshold,
		Instruction:           h.opts.Instruction,
		DeferPostToolReminder: h.agent == kitagenthook.AgentHermes,
	}, nil
}

func (h roborevAgentHookHandler) post(
	ctx context.Context,
	req agenthook.Request,
) (agenthook.Response, bool) {
	resp, err := postAgentHook(ctx, h.opts.RoborevServerAddr, req)
	if err != nil {
		fmt.Fprintf(h.stderr, "roborev agent-hook: %v\n", err)
		return agenthook.Response{}, false
	}
	return resp, true
}

func (h roborevAgentHookHandler) PreToolUse(
	ctx context.Context,
	input kitagenthook.PreToolUseInput,
) (kitagenthook.PreToolUseOutput, error) {
	req, err := h.request(input.CommonInput, input.ToolName, input.ToolInput)
	if err != nil {
		return kitagenthook.PreToolUseOutput{}, err
	}
	req.Event.ToolUseID = input.ToolUseID
	h.post(ctx, req)
	return kitagenthook.PreToolUseOutput{}, nil
}

func (h roborevAgentHookHandler) PostToolUse(
	ctx context.Context,
	input kitagenthook.PostToolUseInput,
) (kitagenthook.PostToolUseOutput, error) {
	req, err := h.request(input.CommonInput, input.ToolName, input.ToolInput)
	if err != nil {
		return kitagenthook.PostToolUseOutput{}, err
	}
	req.Event.ToolUseID = input.ToolUseID
	req.Event.ToolResponse = input.ToolResponse
	resp, ok := h.post(ctx, req)
	if !ok || !resp.Triggered || h.agent == kitagenthook.AgentCursor || h.agent == kitagenthook.AgentHermes {
		return kitagenthook.PostToolUseOutput{}, nil
	}
	return kitagenthook.PostToolUseOutput{
		AdditionalContext: agenthook.PostToolUseAdditionalContextWithFixGuidelines(
			resp.Reason, h.opts.FixGuidelines,
		),
	}, nil
}

func (h roborevAgentHookHandler) Stop(
	ctx context.Context,
	input kitagenthook.StopInput,
) (kitagenthook.StopOutput, error) {
	req, err := h.request(input.CommonInput, "", nil)
	if err != nil {
		return kitagenthook.StopOutput{}, err
	}
	req.Event.StopHookActive = input.StopHookActive
	req.Event.LastAssistant = input.LastAssistantMessage
	resp, ok := h.post(ctx, req)
	if !ok || !resp.Triggered || h.agent == kitagenthook.AgentCursor {
		return kitagenthook.StopOutput{}, nil
	}
	return kitagenthook.StopOutput{
		Decision: kitagenthook.DecisionBlock,
		Reason:   agenthook.StopReasonWithFixGuidelines(resp.Reason, h.opts.FixGuidelines),
	}, nil
}
