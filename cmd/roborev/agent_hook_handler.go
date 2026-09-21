package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"

	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/skills"
)

const agentHookSkillInstallCommand = "roborev skills install"

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
	toolInput jsontext.Value,
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
		MCP:                   h.opts.MCP,
		Agent:                 h.agent,
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
		AdditionalContext: prependAgentHookFixSkillWarning(
			h.agent, h.opts.MCP,
			agenthook.PostToolUseAdditionalContextWithFixGuidelines(
				resp.Reason, h.opts.FixGuidelines,
			),
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
	if resp.TriggeredBy == "fix_session" {
		return kitagenthook.StopOutput{
			Decision: kitagenthook.DecisionBlock,
			Reason:   agenthook.StopReason(resp.Reason),
		}, nil
	}
	return kitagenthook.StopOutput{
		Decision: kitagenthook.DecisionBlock,
		Reason: prependAgentHookFixSkillWarning(
			h.agent, h.opts.MCP,
			agenthook.StopReasonWithFixGuidelines(
				resp.Reason, h.opts.FixGuidelines,
			),
		),
	}, nil
}

func prependAgentHookFixSkillWarning(agent kitagenthook.Agent, mcp bool, instruction string) string {
	skillAgent, supported := mapAgentHookSkillAgent(agent)
	if !supported {
		return instruction
	}

	status, found := skills.StatusForAgent(skillAgent)
	if !found {
		return instruction
	}
	installCommand := agentHookSkillInstallCommand
	if mcp {
		installCommand += " --mcp"
	} else if status.MCP {
		installCommand += " --mcp=false"
	}
	state, installed := status.Skills["roborev-fix"]
	if !installed {
		state = skills.SkillMissing
	}
	if state == skills.SkillCurrent && status.MCP != mcp {
		state = skills.SkillOutdated
	}
	switch state {
	case skills.SkillMissing:
		return fmt.Sprintf(
			"Warning: the roborev-fix skill is missing. Run '%s' before following this reminder.\n\n%s",
			installCommand, instruction,
		)
	case skills.SkillOutdated:
		return fmt.Sprintf(
			"Warning: the installed roborev-fix skill is outdated. Run '%s' before following this reminder.\n\n%s",
			installCommand, instruction,
		)
	default:
		return instruction
	}
}

func mapAgentHookSkillAgent(agent kitagenthook.Agent) (skills.Agent, bool) {
	for _, supported := range skills.Agents() {
		if string(supported) == string(agent) {
			return supported, true
		}
	}
	return "", false
}
