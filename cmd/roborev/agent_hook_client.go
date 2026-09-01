package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
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
		ctx, ep, http.MethodPost, "/api/agent-hook/event", req,
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
		ctx, ep, http.MethodPost, "/api/agent-hook/fix-done",
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
		context.Background(), ep, http.MethodGet, "/api/agent-hook/sessions", nil,
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
		context.Background(), ep, http.MethodPost, "/api/agent-hook/reset",
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
	method, path string,
	reqBody any,
) ([]byte, error) {
	var body io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, ep.BaseURL()+path, body)
	if err != nil {
		return nil, err
	}
	if reqBody != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := ep.HTTPClient(5 * time.Second).Do(httpReq)
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
