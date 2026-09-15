package main

import (
	"context"
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"
	"uuid"

	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/daemon"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

var (
	postAgentHook         = postAgentHookRequest
	agentHookEnsureDaemon = ensureDaemon
)

//go:embed agent_hook_fix_reason.md.gotmpl
var agentHookFixReasonText string

var agentHookFixReasonTemplate = template.Must(
	template.New("agent-hook-fix-reason").Parse(agentHookFixReasonText),
)

func postAgentHookRequest(
	ctx context.Context,
	addr string,
	req agenthook.Request,
) (agenthook.Response, error) {
	ep, err := agentHookEndpoint(addr)
	if err != nil {
		return agenthook.Response{}, err
	}
	body, err := doAgentHookRequest(
		ctx, ep, func(ctx context.Context, api *roborevclient.Client, body []byte) (*http.Response, error) {
			return api.RecordAgentHookEventRaw(ctx, nil, roborevclient.WithBody(body))
		}, req,
	)
	if err != nil {
		return agenthook.Response{}, err
	}
	var out agenthook.Response
	if err := json.Unmarshal(body, &out); err != nil {
		return agenthook.Response{}, err
	}
	if out.FixSessionID != nil {
		executable, err := os.Executable()
		if err != nil {
			return agenthook.Response{}, fmt.Errorf("resolve roborev executable: %w", err)
		}
		args := []string{"agent-hook", "fix-done"}
		if addr != "" {
			args = append(args, "--roborev-server", addr)
		}
		args = append(args, out.FixSessionID.String())
		commands, err := kitagenthook.BuildCommand(executable, args...)
		if err != nil {
			return agenthook.Response{}, fmt.Errorf("build fix completion command: %w", err)
		}
		var rendered strings.Builder
		if err := agentHookFixReasonTemplate.Execute(&rendered, struct {
			Reason  string
			Command string
		}{strings.TrimSpace(out.Reason), commands.Native}); err != nil {
			return agenthook.Response{}, fmt.Errorf("render fix completion instruction: %w", err)
		}
		out.Reason = strings.TrimSpace(rendered.String())
	}
	return out, nil
}

func postAgentHookFixDoneRequest(
	ctx context.Context,
	addr string,
	fixSessionID uuid.UUID,
) error {
	ep, err := agentHookEndpoint(addr)
	if err != nil {
		return err
	}
	_, err = doAgentHookRequest(
		ctx, ep, func(ctx context.Context, api *roborevclient.Client, body []byte) (*http.Response, error) {
			return api.CompleteAgentHookFixRaw(ctx, nil, roborevclient.WithBody(body))
		},
		daemon.AgentHookFixDoneRequest{FixSessionID: fixSessionID},
	)
	return err
}

func runAgentHookStatus(stdout io.Writer) error {
	if err := agentHookEnsureDaemon(); err != nil {
		return err
	}
	ep, err := agentHookEndpoint("")
	if err != nil {
		return err
	}
	body, err := doAgentHookRequest(
		context.Background(), ep, func(ctx context.Context, api *roborevclient.Client, _ []byte) (*http.Response, error) {
			return api.ListAgentHookSessionsRaw(ctx)
		}, nil,
	)
	if err != nil {
		return err
	}
	_, err = stdout.Write(body)
	return err
}

func runAgentHookReset(opts agenthook.ResetOptions, sessionID string, stdout io.Writer) error {
	if !opts.All && sessionID == "" {
		return fmt.Errorf("reset requires a session id or --all")
	}
	if err := agentHookEnsureDaemon(); err != nil {
		return err
	}
	ep, err := agentHookEndpoint("")
	if err != nil {
		return err
	}
	body, err := doAgentHookRequest(
		context.Background(), ep, func(ctx context.Context, api *roborevclient.Client, body []byte) (*http.Response, error) {
			return api.ResetAgentHookSessionsRaw(ctx, nil, roborevclient.WithBody(body))
		},
		map[string]any{"all": opts.All, "session_id": sessionID},
	)
	if err != nil {
		return err
	}
	_, err = stdout.Write(body)
	return err
}

func doAgentHookRequest(
	ctx context.Context,
	ep daemon.DaemonEndpoint,
	call func(context.Context, *roborevclient.Client, []byte) (*http.Response, error),
	reqBody any,
) ([]byte, error) {
	var body []byte
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		body = encoded
	}
	resp, err := call(ctx, ep.APIClient(5*time.Second), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"roborev daemon returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(responseBody)),
		)
	}
	return responseBody, readErr
}

func agentHookEndpoint(addr string) (daemon.DaemonEndpoint, error) {
	if strings.TrimSpace(addr) == "" {
		return getDaemonEndpoint(), nil
	}
	ep, err := daemon.ParseEndpoint(addr)
	if err != nil {
		return daemon.DaemonEndpoint{}, fmt.Errorf("parse roborev daemon address: %w", err)
	}
	return ep, nil
}
