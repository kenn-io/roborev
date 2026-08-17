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
	return out, nil
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
