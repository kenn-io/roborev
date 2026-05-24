package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Usage holds token consumption data for a single review job.
// Stored as JSON in the review_jobs.token_usage column.
// Fields align with agentsview's session-usage output.
type Usage struct {
	OutputTokens      int64   `json:"total_output_tokens,omitempty"`
	PeakContextTokens int64   `json:"peak_context_tokens,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
	HasCost           bool    `json:"has_cost,omitempty"`
}

// agentsviewResponse is the JSON shape returned by both
// `agentsview session usage <id> --format json` and the deprecated
// `agentsview token-use <id>`. The session-usage shape is a strict
// superset of token-use, adding the cost fields.
type agentsviewResponse struct {
	SessionID         string  `json:"session_id"`
	Agent             string  `json:"agent"`
	Project           string  `json:"project"`
	OutputTokens      int64   `json:"total_output_tokens"`
	PeakContextTokens int64   `json:"peak_context_tokens"`
	CostUSD           float64 `json:"cost_usd"`
	HasCost           bool    `json:"has_cost"`
}

// FormatSummary returns a compact human-readable summary like
// "118.0k ctx · 28.8k out · ~$0.42". The cost segment is appended only
// when a cost estimate is available. Returns empty string when there
// is neither token nor cost data.
func (u Usage) FormatSummary() string {
	hasTokens := u.PeakContextTokens != 0 || u.OutputTokens != 0
	if !hasTokens {
		// No token counts: show the cost alone when present.
		return u.FormatCost()
	}
	s := fmt.Sprintf(
		"%s ctx · %s out",
		formatCount(u.PeakContextTokens),
		formatCount(u.OutputTokens),
	)
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

// capability describes which agentsview usage command the installed
// binary supports.
type capability int

const (
	// capNone means agentsview is missing or too old to query.
	capNone capability = iota
	// capTokenUse means only the deprecated `token-use` command is
	// available.
	capTokenUse
	// capSessionUsage means `session usage` is available. It returns a
	// cost estimate and supersedes token-use.
	capSessionUsage
)

// minVersion is the minimum agentsview version that supports the
// (deprecated) token-use subcommand (0.15.0).
var minVersion = [3]int{0, 15, 0}

// sessionUsageMinVersion is the tagged agentsview version that
// supports `session usage`, which returns a cost estimate and replaces
// token-use. Some prerelease builds below this tag also support the
// command, so resolveAgentsview probes for it before falling back.
var sessionUsageMinVersion = [3]int{0, 30, 0}

// versionRe extracts major.minor.patch from "agentsview vX.Y.Z...".
var versionRe = regexp.MustCompile(
	`agentsview v(\d+)\.(\d+)\.(\d+)`,
)

// geVersion reports whether version a is >= version b, comparing
// major, then minor, then patch.
func geVersion(a, b [3]int) bool {
	for i := range 3 {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// parseVersion inspects the output of `agentsview version` and reports
// the capability level it supports plus whether a version string was
// found at all. parsed is false only when no version could be matched,
// so callers can distinguish "too old" from "unparseable" and retry
// the latter.
func parseVersion(out []byte) (level capability, parsed bool) {
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return capNone, false
	}
	var ver [3]int
	for i := range 3 {
		ver[i], _ = strconv.Atoi(string(m[i+1]))
	}
	switch {
	case geVersion(ver, sessionUsageMinVersion):
		return capSessionUsage, true
	case geVersion(ver, minVersion):
		return capTokenUse, true
	default:
		return capNone, true
	}
}

var (
	versionMu     sync.Mutex
	cachedChecked bool
	cachedCap     capability
	cachedBin     string
)

// ResetVersionCache clears the cached version check result.
// Exposed for testing only.
func ResetVersionCache() {
	versionMu.Lock()
	defer versionMu.Unlock()
	cachedChecked = false
	cachedCap = capNone
	cachedBin = ""
}

// resolveAgentsview checks whether agentsview is installed and which
// usage capability it supports. The result is cached keyed to the
// resolved binary path, so a PATH change triggers a fresh probe.
// Transient failures (binary not found, timeout, exec error,
// unparseable output) leave the cache unchecked so the next call
// retries. Returns ("", capNone) when agentsview cannot be used.
func resolveAgentsview(ctx context.Context) (string, capability) {
	// LookPath is cheap (PATH scan, no exec) — always run it so we
	// detect installs and PATH changes.
	bin, err := exec.LookPath("agentsview")
	if err != nil {
		return "", capNone
	}

	versionMu.Lock()
	if cachedChecked && cachedBin == bin {
		level := cachedCap
		versionMu.Unlock()
		if level == capNone {
			return "", capNone
		}
		return bin, level
	}
	versionMu.Unlock()

	// Exec runs without holding the lock so concurrent callers are not
	// blocked by the 5 s command timeout.
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, bin, "version").Output()
	if err != nil {
		return "", capNone
	}

	level, parsed := parseVersion(out)
	if level == capTokenUse && hasSessionUsageCommand(ctx, bin) {
		level = capSessionUsage
	}

	versionMu.Lock()
	defer versionMu.Unlock()

	// Re-check: another goroutine may have updated the cache.
	if cachedChecked && cachedBin == bin {
		if cachedCap == capNone {
			return "", capNone
		}
		return bin, cachedCap
	}

	// Only cache a parsed result. Unparseable output (parsed=false) is
	// treated as transient and left unchecked so the next call retries.
	if parsed {
		cachedChecked = true
		cachedCap = level
		cachedBin = bin
	}
	if level == capNone {
		return "", capNone
	}
	return bin, level
}

func hasSessionUsageCommand(ctx context.Context, binPath string) bool {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		cmdCtx, binPath, "session", "usage", "--help",
	)
	return cmd.Run() == nil
}

// FetchForSession queries agentsview for a session's token usage and
// cost estimate. It calls `session usage` when supported and falls back
// to the deprecated `token-use` on older versions. Returns nil (no
// error) when agentsview is not installed, is too old, or the session
// has no usage data.
func FetchForSession(
	ctx context.Context, sessionID string,
) (*Usage, error) {
	if sessionID == "" {
		return nil, nil
	}

	binPath, level := resolveAgentsview(ctx)
	if level == capNone {
		return nil, nil
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if level == capSessionUsage {
		cmd = exec.CommandContext(
			cmdCtx, binPath, "session", "usage", sessionID,
			"--format", "json",
		)
	} else {
		cmd = exec.CommandContext(
			cmdCtx, binPath, "token-use", sessionID,
		)
	}

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// agentsview usage exit codes: 2 = session not found,
			// 3 = found but no token/cost data. Both mean "no usage",
			// not an error. Legacy token-use (< 0.30.0) signalled
			// not-found with exit 1 and empty stdout+stderr.
			switch code := exitErr.ExitCode(); {
			case code == 2 || code == 3:
				return nil, nil
			case code == 1 && len(out) == 0 && len(exitErr.Stderr) == 0:
				return nil, nil
			default:
				return nil, fmt.Errorf(
					"agentsview usage: exit %d: %s",
					code, exitErr.Stderr,
				)
			}
		}
		return nil, fmt.Errorf("agentsview usage: %w", err)
	}

	var resp agentsviewResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse agentsview output: %w", err)
	}

	if resp.OutputTokens == 0 && resp.PeakContextTokens == 0 &&
		!resp.HasCost {
		return nil, nil
	}
	return &Usage{
		OutputTokens:      resp.OutputTokens,
		PeakContextTokens: resp.PeakContextTokens,
		CostUSD:           resp.CostUSD,
		HasCost:           resp.HasCost,
	}, nil
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
	if u.OutputTokens == 0 && u.PeakContextTokens == 0 && !u.HasCost {
		return nil
	}
	return &u
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
