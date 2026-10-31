package provenance

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"unicode/utf8"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/tokens"
)

var ErrUnsupported = errors.New("agentsview commit lookup is unsupported")

// ErrLookupTooLarge is the shared protocol's atomic result-bound failure.
var ErrLookupTooLarge = errors.New("agentsview commit lookup exceeds result limits")

// These are the shared commits/sessions HTTP protocol bounds, settled in the
// commit-provenance design, not prompt clipping or local workload limits.
const (
	maxRequestSHAs   = 200
	maxResponseBytes = 8 * 1024 * 1024
	maxResponseLinks = 4096
)

// Client reuses capability results across worker builds. A zero Client works.
// Config and tokens are resolved for every call, including token-file rotation.
type Client struct {
	HTTPClient  *http.Client
	Command     string
	unsupported sync.Map
}

func validSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (c *Client) Lookup(ctx context.Context, repo string, shas []string, cfg *config.Config) (LookupResult, error) {
	result := LookupResult{Commits: map[string][]Link{}, Coverage: map[string]string{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	fetch, err := tokens.FetchConfigFromConfig(cfg)
	if err != nil {
		return result, errors.New("agentsview credentials unavailable")
	}
	for _, sha := range shas {
		if !validSHA(sha) {
			return result, errors.New("invalid commit lookup SHA")
		}
	}
	if fetch.AgentsViewURL == "" && fetch.Endpoint != "" {
		return result, ErrUnsupported
	}
	key := fetch.AgentsViewURL
	bin := c.Command
	if key == "" {
		if bin == "" {
			bin, err = exec.LookPath("agentsview")
			if err != nil {
				return result, ErrUnsupported
			}
		}
		key = "cli:" + bin
	}
	if _, disabled := c.unsupported.Load(key); disabled {
		return result, ErrUnsupported
	}
	var errs []error
	if fetch.AgentsViewURL != "" {
		// An empty batch is the protocol's side-effect-free capability probe.
		for start := 0; start < len(shas) || start == 0; start += maxRequestSHAs {
			end := min(start+maxRequestSHAs, len(shas))
			batch := append([]string{}, shas[start:end]...)
			requestCtx, cancel := context.WithTimeout(ctx, cfg.Cost.ResolvedTimeout())
			links, lookupErr := c.http(requestCtx, fetch, batch)
			cancel()
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if lookupErr != nil {
				errs = append(errs, lookupErr)
				if errors.Is(lookupErr, ErrUnsupported) {
					c.unsupported.Store(key, true)
					break
				}
			} else {
				for _, sha := range batch {
					result.Commits[sha] = links[sha]
					result.Coverage[sha] = "http"
				}
			}
			if end == len(shas) {
				break
			}
		}
	} else {
		for _, sha := range shas {
			commandCtx, cancel := context.WithTimeout(ctx, cfg.Cost.ResolvedTimeout())
			links, lookupErr := runWhy(commandCtx, bin, repo, sha)
			providerTimedOut := errors.Is(commandCtx.Err(), context.DeadlineExceeded)
			cancel()
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if lookupErr != nil {
				errs = append(errs, lookupErr)
				if errors.Is(lookupErr, ErrUnsupported) {
					c.unsupported.Store(key, true)
					break
				}
				if providerTimedOut {
					// Keep results collected so far; prompt building can still use
					// local trailers without spending a timeout on every remaining SHA.
					break
				}
			} else {
				result.Commits[sha] = links
				result.Coverage[sha] = "why-selected"
			}
		}
	}
	return result, errors.Join(errs...)
}

func (c *Client) http(ctx context.Context, fetch tokens.FetchConfig, shas []string) (map[string][]Link, error) {
	body, _ := json.Marshal(struct {
		SHAs []string `json:"shas"`
	}{SHAs: shas})
	endpoint := strings.TrimRight(fetch.AgentsViewURL, "/") + "/api/v1/commits/sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid agentsview lookup URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if fetch.AgentsViewToken != "" {
		req.Header.Set("Authorization", "Bearer "+fetch.AgentsViewToken)
	}
	client := http.DefaultClient
	if c.HTTPClient != nil {
		client = c.HTTPClient
	}
	// Do not forward an archive credential to redirect targets, including
	// another origin on the same hostname. This API has no redirect contract.
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := copyClient.Do(req)
	if err != nil {
		return nil, errors.New("agentsview commit lookup request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrUnsupported
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, ErrLookupTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agentsview commit lookup HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, errors.New("read agentsview commit lookup response")
	}
	if len(data) > maxResponseBytes {
		return nil, ErrLookupTooLarge
	}
	var payload struct {
		Commits map[string][]Link `json:"commits"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Commits == nil {
		return nil, errors.New("invalid agentsview commit lookup response")
	}
	count := 0
	for _, links := range payload.Commits {
		count += len(links)
		for _, link := range links {
			if err := validateLink(link); err != nil {
				return nil, err
			}
		}
	}
	if count > maxResponseLinks {
		return nil, ErrLookupTooLarge
	}
	base, _ := url.Parse(strings.TrimRight(fetch.AgentsViewURL, "/") + "/")
	for _, sha := range shas {
		links, ok := payload.Commits[sha]
		if !ok || links == nil {
			return nil, errors.New("agentsview commit lookup omitted a request key")
		}
		for i := range links {
			links[i].WebURL = browserURL(base, links[i].WebURL)
		}
	}
	return payload.Commits, nil
}

func browserURL(base *url.URL, raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.User != nil {
		return ""
	}
	if u.IsAbs() && u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.IsAbs() && u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func runWhy(ctx context.Context, bin, repo, sha string) ([]Link, error) {
	out, err := tokens.RunAgentsviewCommand(ctx, repo, bin, "why", "--commit", sha, "--format", "json", "--no-sync")
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && unsupportedCLI(string(exit.Stderr)) {
			return nil, ErrUnsupported
		}
		return nil, errors.New("agentsview why lookup failed")
	}
	var payload struct {
		Refs []struct {
			Key  string `json:"key"`
			How  string `json:"how"`
			Card Link   `json:"card"`
		} `json:"refs"`
	}
	if err := json.Unmarshal(out, &payload); err != nil || payload.Refs == nil {
		return nil, errors.New("invalid agentsview why response")
	}
	var links []Link
	for _, ref := range payload.Refs {
		if ref.How == "direct" && ref.Key == "session:"+ref.Card.SessionID {
			if err := validateLink(ref.Card); err != nil {
				return nil, err
			}
			ref.Card.WebURL = browserURL(nil, ref.Card.WebURL)
			links = append(links, ref.Card)
		}
	}
	return links, nil
}

func unsupportedCLI(stderr string) bool {
	return strings.Contains(stderr, "unknown command") || strings.Contains(stderr, "unknown flag")
}

// Probe never reads sessions: HTTP uses an empty batch, CLI requests help.
func (c *Client) Probe(ctx context.Context, repo string, cfg *config.Config) error {
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if cfg.AgentsView.URL != "" || cfg.Cost.Endpoint != "" {
		_, err := c.Lookup(ctx, repo, nil, cfg)
		return err
	}
	bin := c.Command
	if bin == "" {
		var err error
		bin, err = exec.LookPath("agentsview")
		if err != nil {
			return ErrUnsupported
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Cost.ResolvedTimeout())
	defer cancel()
	out, err := tokens.RunAgentsviewCommand(ctx, repo, bin, "why", "--help", "--no-sync")
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && unsupportedCLI(string(exit.Stderr)) {
			return ErrUnsupported
		}
		return errors.New("agentsview capability probe failed")
	}
	if !strings.Contains(string(out), "--commit") {
		return ErrUnsupported
	}
	return nil
}

// Validate only the shared wire contract, without tightening its nullable
// transcript coordinates or rejecting provider metadata outside our subset.
func validateLink(link Link) error {
	invalid := errors.New("invalid agentsview commit-session record")
	agent, ok := sessionAgent(link.SessionID)
	if !ok || !validSHA(link.CommitSHA) || (link.Resolved && len(link.CommitSHA) != 40) {
		return invalid
	}
	if link.Source != "trailer" && link.Source != "transcript" {
		return invalid
	}
	if link.MessageOrdinal != nil && *link.MessageOrdinal < 0 || link.CallIndex != nil && *link.CallIndex < 0 {
		return invalid
	}
	if link.Source == "trailer" && (link.MessageOrdinal != nil || link.CallIndex != nil) {
		return invalid
	}
	if link.Session == nil {
		if !link.Gone || link.Turn != nil || link.WebURL != "" || link.Agent != "" {
			return invalid
		}
	} else if link.Gone || link.Session.ID != link.SessionID || link.Agent != agent {
		return invalid
	}
	if turn := link.Turn; turn != nil {
		if turn.Ordinal < 0 || utf8.RuneCountInString(turn.Prompt) > 600 {
			return invalid
		}
		switch turn.Why {
		case "commit-call", "wrote", "before", "match":
		default:
			return invalid
		}
	}
	return nil
}
