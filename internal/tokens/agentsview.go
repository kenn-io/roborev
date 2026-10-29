package tokens

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// FetchConfigFromConfig keeps all daemon lookups and diagnostics on one provider.
// cfg is validated at load; resolve the token here to observe file rotation.
func FetchConfigFromConfig(cfg *config.Config) (FetchConfig, error) {
	out := FetchConfig{Endpoint: cfg.Cost.Endpoint, Timeout: cfg.Cost.ResolvedTimeout(), RequireCLI: true, AgentsViewURL: cfg.AgentsView.URL}
	if out.AgentsViewURL != "" {
		token, err := cfg.AgentsView.ResolveToken()
		if err != nil {
			return FetchConfig{}, err
		}
		out.AgentsViewToken = token
	}
	return out, nil
}

func (cfg FetchConfig) request(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if cfg.AgentsViewURL != "" && cfg.AgentsViewToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.AgentsViewToken)
	}
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}

func (cfg FetchConfig) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func agentsViewUsageURL(base, id string) string {
	return strings.TrimRight(base, "/") + "/api/v1/sessions/" + url.PathEscape(id) + "/usage?subagents=true"
}

func agentsViewStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("AgentsView rejected the token (HTTP %d)", resp.StatusCode)
	case http.StatusConflict:
		var body struct {
			Error struct {
				Candidates []string `json:"candidates"`
			} `json:"error"`
		}
		_ = json.UnmarshalRead(resp.Body, &body)
		return fmt.Errorf("AgentsView ambiguous session ID; candidates: %q", body.Error.Candidates)
	default:
		return fmt.Errorf("AgentsView HTTP %d", resp.StatusCode)
	}
}

func fetchForSessionAgentsView(ctx context.Context, id string, cfg FetchConfig) (*Usage, error) {
	if id == "" {
		return nil, nil
	}
	ctx, cancel := cfg.requestContext(ctx)
	defer cancel()
	usage, missing, err := cfg.agentsViewUsage(ctx, id)
	if err != nil || !missing {
		return usage, err
	}
	query := url.Values{"partial": {id}, "raw_suffix": {"true"}, "limit": {"2"}}
	endpoint := strings.TrimRight(cfg.AgentsViewURL, "/") + "/api/v1/session-ids/resolve?" + query.Encode()
	resp, err := cfg.request(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("AgentsView resolve request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := agentsViewStatus(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AgentsView resolve HTTP %d", resp.StatusCode)
	}
	var resolved struct {
		IDs       []string `json:"ids"`
		RawSuffix bool     `json:"raw_suffix"`
	}
	if err := json.UnmarshalRead(resp.Body, &resolved); err != nil {
		return nil, fmt.Errorf("parse AgentsView resolve output")
	}
	// AgentsView v0.46.1 acknowledges literal raw-suffix matching. Older
	// resolvers may ignore the query and return unrelated substring matches.
	if !resolved.RawSuffix {
		return nil, fmt.Errorf("AgentsView server does not acknowledge raw session ID lookup")
	}
	switch len(resolved.IDs) {
	case 0:
		return nil, nil
	case 1:
		usage, _, err = cfg.agentsViewUsage(ctx, resolved.IDs[0])
		return usage, err
	default:
		return nil, fmt.Errorf("AgentsView ambiguous session ID; candidates: %q", resolved.IDs)
	}
}

func (cfg FetchConfig) agentsViewUsage(ctx context.Context, id string) (*Usage, bool, error) {
	resp, err := cfg.request(ctx, agentsViewUsageURL(cfg.AgentsViewURL, id))
	if err != nil {
		return nil, false, fmt.Errorf("AgentsView usage request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := agentsViewStatus(resp); err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, true, nil
	}
	var payload SessionUsagePayload
	if err := json.UnmarshalRead(resp.Body, &payload); err != nil {
		return nil, false, fmt.Errorf("parse AgentsView usage output")
	}
	usage, err := UsageFromSessionPayload(payload)
	return usage, false, err
}

// ProbeAgentsView checks reachability and authentication without reading usage.
func (cfg FetchConfig) ProbeAgentsView(ctx context.Context) error {
	ctx, cancel := cfg.requestContext(ctx)
	defer cancel()
	resp, err := cfg.request(ctx, strings.TrimRight(cfg.AgentsViewURL, "/")+"/api/ping")
	if err != nil {
		return fmt.Errorf("AgentsView unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var ping struct {
			OK      bool   `json:"ok"`
			Service string `json:"service"`
		}
		if err := json.UnmarshalRead(resp.Body, &ping); err != nil || !ping.OK || ping.Service != "agentsview" {
			return fmt.Errorf("AgentsView ping did not return the expected service identity")
		}
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return agentsViewStatus(resp)
	}
	return fmt.Errorf("AgentsView ping HTTP %d", resp.StatusCode)
}
