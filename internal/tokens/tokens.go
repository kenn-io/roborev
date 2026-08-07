package tokens

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/procutil"
)

// ErrUsageProviderUnavailable marks a required usage provider that cannot be
// invoked. Callers can distinguish it from a missing session without treating
// the optional provider as a user-visible failure.
var ErrUsageProviderUnavailable = errors.New("usage provider unavailable")

// Usage holds token consumption data for a single review job.
// Stored as JSON in the review_jobs.token_usage column.
// Fields align with agentsview's session-usage output.
type Usage struct {
	InputTokens int64 `json:"input_tokens,omitempty"`
	// CachedInputTokens counts cache *reads*. Note the provider difference:
	// OpenAI's input_tokens includes cached_input_tokens, while Anthropic
	// reports the two as disjoint sets.
	CachedInputTokens int64 `json:"cached_input_tokens,omitempty"`
	// CacheCreationTokens counts tokens written into the prompt cache, which
	// is priced separately from cache reads.
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	OutputTokens        int64 `json:"total_output_tokens,omitempty"`
	PeakContextTokens   int64 `json:"peak_context_tokens,omitempty"`
	// CostUSD is written unconditionally. Omitting a zero would make a run
	// priced at exactly $0 byte-identical to the drifted rows that recorded
	// has_cost with no amount, which is the ambiguity this field exists to
	// resolve.
	CostUSD     float64 `json:"cost_usd"`
	HasCost     bool    `json:"has_cost,omitempty"`
	UsageSource string  `json:"usage_source,omitempty"`
	ThreadID    string  `json:"thread_id,omitempty"`
	EventOffset int64   `json:"event_offset,omitempty"`
}

// FetchConfig configures session usage lookup. When Endpoint is set,
// roborev fetches usage over HTTP using a URL template containing
// "{session_id}". An empty Endpoint preserves the agentsview CLI path.
type FetchConfig struct {
	Endpoint string
	Timeout  time.Duration
	Client   *http.Client
	// RequireCLI reports a missing agentsview binary as an error when
	// Endpoint is empty. By default CLI lookup is best-effort.
	RequireCLI bool
}

// agentsviewResponse is the JSON shape returned by
// `agentsview session usage <id> --format json` and the deprecated
// `agentsview token-use <id>` command.
type agentsviewResponse struct {
	SessionID         string        `json:"session_id"`
	Agent             string        `json:"agent"`
	Project           string        `json:"project"`
	OutputTokens      int64         `json:"total_output_tokens"`
	PeakContextTokens int64         `json:"peak_context_tokens"`
	HasTokenData      bool          `json:"has_token_data"`
	CostUSD           *float64      `json:"cost_usd"`
	Cost              *costEnvelope `json:"cost"`
	HasCost           bool          `json:"has_cost"`
}

// costEnvelope is the integer-cost shape agentsview adopted in v0.39.0:
// `"cost": {"microdollars": 131767}`. Microdollars is a pointer so an explicit
// null is distinguishable from a genuine zero-cost run.
type costEnvelope struct {
	Microdollars *int64 `json:"microdollars,omitempty"`
}

// resolveCostUSD converts either cost shape to dollars, reporting whether a
// cost was actually present. An explicit `"cost_usd": null` is absent, not $0;
// a real 0 (or 0 microdollars) is a valid free run. The microdollar envelope
// wins when both are populated because it is the current agentsview format.
func resolveCostUSD(costUSD *float64, cost *costEnvelope) (float64, bool) {
	if cost != nil && cost.Microdollars != nil {
		return float64(*cost.Microdollars) / 1e6, true
	}
	if costUSD != nil {
		return *costUSD, true
	}
	return 0, false
}

// SessionUsagePayload is the JSON shape returned by AgentsView's
// session-usage API and accepted by roborev's token backfill endpoint.
type SessionUsagePayload struct {
	SessionID         string        `json:"session_id"`
	Agent             string        `json:"agent,omitempty"`
	Project           string        `json:"project,omitempty"`
	InputTokens       *int64        `json:"input_tokens,omitempty"`
	CachedInputTokens *int64        `json:"cached_input_tokens,omitempty"`
	OutputTokens      *int64        `json:"total_output_tokens,omitempty"`
	PeakContextTokens *int64        `json:"peak_context_tokens,omitempty"`
	HasTokenData      *bool         `json:"has_token_data"`
	CostUSD           *float64      `json:"cost_usd,omitempty"`
	Cost              *costEnvelope `json:"cost,omitempty"`
	HasCost           *bool         `json:"has_cost"`
}

// FormatSummary returns a compact human-readable summary like
// "118.0k ctx · 28.8k out · ~$0.42". The cost segment is appended only
// when a cost estimate is available. Returns empty string when there
// is neither token nor cost data.
func (u Usage) FormatSummary() string {
	inputTokens := u.PeakContextTokens
	inputLabel := "ctx"
	if inputTokens == 0 && u.InputTokens != 0 {
		inputTokens = u.InputTokens
		inputLabel = "in"
	}
	hasTokens := inputTokens != 0 || u.OutputTokens != 0 ||
		u.CachedInputTokens != 0 || u.CacheCreationTokens != 0
	if !hasTokens {
		// No token counts: show the cost alone when present.
		return u.FormatCost()
	}
	// Cache reads and cache writes are priced differently, so they are
	// reported separately rather than summed.
	var notes []string
	if u.CachedInputTokens != 0 {
		notes = append(notes, formatCount(u.CachedInputTokens)+" cached")
	}
	if u.CacheCreationTokens != 0 {
		notes = append(notes, formatCount(u.CacheCreationTokens)+" written")
	}
	s := fmt.Sprintf("%s %s", formatCount(inputTokens), inputLabel)
	if len(notes) > 0 {
		s += " (" + strings.Join(notes, ", ") + ")"
	}
	s += " · " + formatCount(u.OutputTokens) + " out"
	if cost := u.FormatCost(); cost != "" {
		s += " · " + cost
	}
	return s
}

// FormatCost returns the cost estimate like "~$0.42", or "" when no
// estimate is available. The tilde marks it as a model-pricing
// estimate, matching agentsview's own rendering.
func (u Usage) FormatCost() string {
	if !u.HasCost {
		return ""
	}
	return fmt.Sprintf("~$%.2f", u.CostUSD)
}

func formatCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func resolveAgentsview() (string, error) {
	return exec.LookPath("agentsview")
}

// FetchForSession queries agentsview for a session's token usage and
// cost estimate. Returns nil (no error) when agentsview is not installed
// or the session has no usage data.
func FetchForSession(
	ctx context.Context, sessionID string,
) (*Usage, error) {
	return FetchForSessionWithConfig(ctx, sessionID, FetchConfig{})
}

// FetchForSessionWithConfig queries a configured HTTP endpoint when
// provided; otherwise it uses the agentsview CLI path.
func FetchForSessionWithConfig(
	ctx context.Context, sessionID string, cfg FetchConfig,
) (*Usage, error) {
	if cfg.Endpoint != "" {
		return fetchForSessionHTTP(ctx, sessionID, cfg)
	}
	return fetchForSessionCLI(ctx, sessionID, cfg)
}

func fetchForSessionCLI(
	ctx context.Context, sessionID string, cfg FetchConfig,
) (*Usage, error) {
	if sessionID == "" {
		return nil, nil
	}

	binPath, err := resolveAgentsview()
	if err != nil {
		if cfg.RequireCLI {
			return nil, fmt.Errorf(
				"%w: agentsview lookup: %w",
				ErrUsageProviderUnavailable, err,
			)
		}
		return nil, nil
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	out, err := runAgentsviewCommand(
		ctx, timeout, binPath,
		"session", "usage", sessionID, "--format", "json",
	)
	if err != nil {
		if shouldFallbackToTokenUse(err) {
			out, err = runAgentsviewCommand(
				ctx, timeout, binPath, "token-use", sessionID,
			)
			if err != nil {
				return nil, handleTokenUseError(out, err)
			}
		} else {
			return nil, handleSessionUsageError(err)
		}
	}

	var resp agentsviewResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse agentsview output: %w", err)
	}

	// A has_cost flag with no accompanying amount carries no dollars, so it is
	// not treated as cost data: recording it would price the review at $0 and
	// silently deflate every aggregate it lands in.
	costUSD, hasCost := 0.0, false
	if resp.HasCost {
		costUSD, hasCost = resolveCostUSD(resp.CostUSD, resp.Cost)
	}

	if resp.OutputTokens == 0 && resp.PeakContextTokens == 0 && !hasCost {
		return nil, nil
	}
	return &Usage{
		OutputTokens:      resp.OutputTokens,
		PeakContextTokens: resp.PeakContextTokens,
		CostUSD:           costUSD,
		HasCost:           hasCost,
	}, nil
}

func buildAgentsviewCmd(ctx context.Context, binPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binPath, args...)
	procutil.HideConsole(cmd)
	return cmd
}

func runAgentsviewCommand(
	ctx context.Context, timeout time.Duration, binPath string, args ...string,
) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return buildAgentsviewCmd(cmdCtx, binPath, args...).Output()
}

func shouldFallbackToTokenUse(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if exitErr.ExitCode() != 1 {
		return false
	}
	stderr := strings.ToLower(string(exitErr.Stderr))
	return strings.Contains(stderr, "unknown command") ||
		strings.Contains(stderr, "unknown subcommand")
}

func handleSessionUsageError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// agentsview usage exit codes: 2 = session not found,
		// 3 = found but no token/cost data. Both mean "no usage",
		// not an error.
		switch code := exitErr.ExitCode(); code {
		case 2, 3:
			return nil
		default:
			return fmt.Errorf(
				"agentsview usage: exit %d: %s",
				code, exitErr.Stderr,
			)
		}
	}
	return fmt.Errorf("agentsview usage: %w", err)
}

func handleTokenUseError(out []byte, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Legacy token-use signalled not-found with exit 1 and empty
		// stdout+stderr.
		if exitErr.ExitCode() == 1 &&
			len(out) == 0 &&
			len(exitErr.Stderr) == 0 {
			return nil
		}
		return fmt.Errorf(
			"agentsview token-use: exit %d: %s",
			exitErr.ExitCode(), exitErr.Stderr,
		)
	}
	return fmt.Errorf("agentsview token-use: %w", err)
}

func fetchForSessionHTTP(
	ctx context.Context, sessionID string, cfg FetchConfig,
) (*Usage, error) {
	if sessionID == "" {
		return nil, nil
	}
	endpoint, err := expandEndpoint(cfg.Endpoint, sessionID)
	if err != nil {
		return nil, err
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("usage endpoint request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage endpoint request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			return nil, fmt.Errorf("usage endpoint HTTP %s", resp.Status)
		}
		return nil, fmt.Errorf("usage endpoint HTTP %s: %s", resp.Status, detail)
	}

	var respBody SessionUsagePayload
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("parse usage endpoint output: %w", err)
	}
	return UsageFromSessionPayload(respBody)
}

func expandEndpoint(template, sessionID string) (string, error) {
	const placeholder = "{session_id}"
	if !strings.Contains(template, placeholder) {
		return "", fmt.Errorf("usage endpoint missing %s placeholder", placeholder)
	}
	endpoint := strings.ReplaceAll(
		template, placeholder, url.PathEscape(sessionID),
	)
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return "", fmt.Errorf("usage endpoint URL: %w", err)
	}
	return endpoint, nil
}

// UsageFromSessionPayload validates an AgentsView session-usage payload
// and converts it to roborev's stored usage shape.
func UsageFromSessionPayload(resp SessionUsagePayload) (*Usage, error) {
	if resp.HasTokenData == nil {
		return nil, fmt.Errorf("usage endpoint schema: missing has_token_data")
	}
	if resp.HasCost == nil {
		return nil, fmt.Errorf("usage endpoint schema: missing has_cost")
	}

	usage := &Usage{}
	if *resp.HasTokenData {
		if resp.OutputTokens == nil {
			return nil, fmt.Errorf("usage endpoint schema: missing token counts")
		}
		usage.OutputTokens = *resp.OutputTokens
		if resp.PeakContextTokens != nil {
			usage.PeakContextTokens = *resp.PeakContextTokens
		}
		if resp.InputTokens != nil {
			usage.InputTokens = *resp.InputTokens
		}
		if resp.CachedInputTokens != nil {
			usage.CachedInputTokens = *resp.CachedInputTokens
		}
		if usage.PeakContextTokens == 0 && usage.InputTokens == 0 {
			return nil, fmt.Errorf("usage endpoint schema: missing token counts")
		}
	}
	if *resp.HasCost {
		costUSD, ok := resolveCostUSD(resp.CostUSD, resp.Cost)
		if !ok {
			return nil, fmt.Errorf(
				"usage endpoint schema: missing cost_usd or cost.microdollars",
			)
		}
		usage.CostUSD = costUSD
		usage.HasCost = true
	}
	if !usage.HasUsageData() {
		return nil, nil
	}
	return usage, nil
}

// ParseJSON deserializes a token_usage JSON blob from the database.
// Returns nil for empty/null values or a blob carrying no usage data.
func ParseJSON(data string) *Usage {
	if data == "" {
		return nil
	}
	var u Usage
	if err := json.Unmarshal([]byte(data), &u); err != nil {
		return nil
	}
	// Historical rows written during the agentsview cost-shape transition can
	// carry has_cost without cost_usd. Normalize that drift at the parse
	// boundary so every consumer agrees that HasCost means an amount was
	// actually recorded. A non-nil RawMessage preserves an explicit zero while
	// treating both an absent key and JSON null as unpriced.
	if u.HasCost {
		var raw struct {
			CostUSD *json.RawMessage `json:"cost_usd"`
		}
		if err := json.Unmarshal([]byte(data), &raw); err != nil ||
			raw.CostUSD == nil {
			u.HasCost = false
		}
	}
	if !u.HasUsageData() {
		return nil
	}
	return &u
}

// HasUsageData reports whether the usage value carries counts or cost data.
func (u Usage) HasUsageData() bool {
	return u.InputTokens != 0 ||
		u.CachedInputTokens != 0 ||
		u.CacheCreationTokens != 0 ||
		u.OutputTokens != 0 ||
		u.PeakContextTokens != 0 ||
		u.HasCost
}

// ParseCodexUsageFile extracts the final Codex turn.completed usage event
// from a raw JSONL job log.
func ParseCodexUsageFile(path string) (*Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	return ParseCodexUsageJSONL(f)
}

// ParseCodexUsageJSONL extracts the final Codex turn.completed usage event
// from an event stream. Non-JSON lines and unrelated JSON events are ignored.
func ParseCodexUsageJSONL(r io.Reader) (*Usage, error) {
	type codexUsageEvent struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id,omitempty"`
		Usage    struct {
			InputTokens int64 `json:"input_tokens"`
			// OpenAI's input_tokens is inclusive of cached_input_tokens; the
			// two are not disjoint. Stored as-is to stay faithful to the
			// source event.
			CachedInputTokens     int64 `json:"cached_input_tokens"`
			CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
			OutputTokens          int64 `json:"output_tokens"`
		} `json:"usage,omitempty"`
	}

	reader := bufio.NewReader(r)
	var offset int64
	var threadID string
	var usage *Usage
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var ev codexUsageEvent
				if json.Unmarshal(trimmed, &ev) == nil {
					if ev.ThreadID != "" {
						threadID = ev.ThreadID
					}
					if ev.Type == "turn.completed" {
						u := Usage{
							InputTokens:         ev.Usage.InputTokens,
							CachedInputTokens:   ev.Usage.CachedInputTokens,
							CacheCreationTokens: ev.Usage.CacheWriteInputTokens,
							OutputTokens:        ev.Usage.OutputTokens,
							UsageSource:         "job_log_turn_completed",
							ThreadID:            threadID,
							EventOffset:         offset,
						}
						if ev.ThreadID != "" {
							u.ThreadID = ev.ThreadID
						}
						if u.HasUsageData() {
							usage = &u
						}
					}
				}
			}
			offset += int64(len(line))
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return usage, nil
		}
		return nil, err
	}
}

// ToJSON serializes token usage to JSON for database storage.
func ToJSON(u *Usage) string {
	if u == nil {
		return ""
	}
	data, err := json.Marshal(u)
	if err != nil {
		return ""
	}
	return string(data)
}
