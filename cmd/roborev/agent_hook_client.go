package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/daemon"
)

var (
	postAgentHook         = postAgentHookRequest
	agentHookEnsureDaemon = ensureDaemon
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
		bareCommand := agentHookFixDoneCommand(*out.FixSessionID, "")
		command := agentHookFixDoneCommand(*out.FixSessionID, addr)
		if strings.Contains(out.Reason, bareCommand) {
			out.Reason = strings.Replace(out.Reason, bareCommand, command, 1)
		} else {
			out.Reason += "\n\nAfter completing this Agent Hook fix, run `" + command + "`."
		}
	}
	return out, nil
}

func agentHookFixDoneCommand(fixSessionID uuid.UUID, addr string) string {
	command := "roborev agent-hook fix-done"
	if addr != "" {
		command += " --roborev-server '" + strings.ReplaceAll(addr, "'", `'\''`) + "'"
	}
	return command + " " + fixSessionID.String()
}

func postAgentHookFixDoneRequest(
	ctx context.Context,
	addr string,
	fixSessionID uuid.UUID,
) (agenthook.FixSession, error) {
	ep, err := agentHookEndpoint(addr)
	if err != nil {
		return agenthook.FixSession{}, err
	}
	body, err := doAgentHookRequest(
		ctx, ep, http.MethodPost, "/api/agent-hook/fix-done",
		daemon.AgentHookFixDoneRequest{FixSessionID: fixSessionID},
	)
	if err != nil {
		return agenthook.FixSession{}, err
	}
	var output daemon.AgentHookFixDoneOutput
	if err := json.Unmarshal(body, &output.Body); err != nil {
		return agenthook.FixSession{}, err
	}
	return output.Body.FixSession, nil
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
