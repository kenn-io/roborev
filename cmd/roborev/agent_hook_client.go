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

var postAgentHook = postAgentHookRequest

func postAgentHookRequest(
	ctx context.Context,
	addr string,
	req agenthook.Request,
) (agenthook.Response, error) {
	ep, err := agentHookEndpoint(addr)
	if err != nil {
		return agenthook.Response{}, err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return agenthook.Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, ep.BaseURL()+"/api/agent-hook/event", bytes.NewReader(body),
	)
	if err != nil {
		return agenthook.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ep.HTTPClient(5 * time.Second).Do(httpReq)
	if err != nil {
		return agenthook.Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return agenthook.Response{}, fmt.Errorf(
			"roborev daemon returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var out agenthook.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agenthook.Response{}, err
	}
	return out, nil
}

func runAgentHookStatus(stdout io.Writer) error {
	ep, err := agentHookEndpoint("")
	if err != nil {
		return err
	}
	resp, err := ep.HTTPClient(5 * time.Second).Get(
		ep.BaseURL() + "/api/agent-hook/sessions",
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"roborev daemon returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	_, err = io.Copy(stdout, resp.Body)
	return err
}

func runAgentHookReset(opts agenthook.ResetOptions, sessionID string, stdout io.Writer) error {
	if !opts.All && sessionID == "" {
		return fmt.Errorf("reset requires a session id or --all")
	}
	ep, err := agentHookEndpoint("")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"all":        opts.All,
		"session_id": sessionID,
	})
	if err != nil {
		return err
	}
	resp, err := ep.HTTPClient(5*time.Second).Post(
		ep.BaseURL()+"/api/agent-hook/reset",
		"application/json", bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"roborev daemon returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	_, err = io.Copy(stdout, resp.Body)
	return err
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
