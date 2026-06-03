package agent

import (
	"strings"
	"time"
)

// LimitKind labels a classified agent error.
type LimitKind int

const (
	LimitKindNone      LimitKind = iota // no rate-limit signal recognized
	LimitKindTransient                  // 429-style; retry locally, no cooldown
	LimitKindQuota                      // hard quota exhaustion (Gemini/Codex today)
	// LimitKindSession is a session-level cap (e.g. Claude 5-hour).
	// Plumbing is in place — daemon cooldown/abort and CLI strict abort
	// both handle it — but defaultLimitRules does not yet produce it for
	// any real agent. ClassifyLimit can return LimitKindSession only via
	// an injected classifier (tests). Production support is pending a
	// captured Claude session-cap message; see the TODO at
	// defaultLimitRules.
	LimitKindSession
)

// LimitClassification is the result of inspecting an agent error.
type LimitClassification struct {
	Kind        LimitKind
	Agent       string        // canonical agent name (caller resolves aliases)
	ResetAt     time.Time     // zero if not parseable from the message
	CooldownFor time.Duration // zero if not parseable; caller applies its own fallback
	Message     string        // raw error text (for logs / user display)
}

// LimitClassifier is the function shape used by callers that want to inject
// a stub in tests.
type LimitClassifier func(agent, errMsg string) LimitClassification

// limitRule is one substring → kind mapping. The Agents slice restricts
// the rule to specific canonical agent names; "*" applies to any agent.
type limitRule struct {
	Agents    []string // canonical agent names; "*" = any
	Substring string   // case-insensitive substring match on the error message
	Kind      LimitKind
}

// defaultLimitRules is the production rule table. The nine quota
// substrings are copied from the original isQuotaError set in
// internal/daemon/worker.go so detection for Gemini and Codex is
// byte-for-byte unchanged.
//
// The transient/outage substrings below were added from captured
// provider outage strings (HTTP 429, stream disconnects, 5xx) and
// remain observation-driven: only wording seen in real failures is
// listed, never speculative phrases that could also match policy or
// config-validation errors. A transient classification only triggers a
// local retry with backoff (no cooldown), so the bar is deliberately
// kept high to avoid retrying deterministic failures forever.
//
// TODO: add a LimitKindSession rule for Claude's 5-hour cap once the
// exact error wording is captured from a real session-cap failure.
// Speculative substrings ("usage limit", "limit reached", etc.) are
// not added on purpose — they would also match policy errors,
// transient 429s, and config-validation messages, and a false positive
// would abort roborev fix and cool down the agent when retrying might
// have worked. Until that pattern lands, ClassifyLimit returns
// LimitKindSession only when an injected classifier produces it
// (i.e., in tests).
var defaultLimitRules = []limitRule{
	{Agents: []string{"*"}, Substring: "resource exhausted", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "quota exceeded", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "quota_exceeded", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "quota exhausted", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "quota_exhausted", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "insufficient_quota", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "exhausted your capacity", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "capacity exhausted", Kind: LimitKindQuota},
	{Agents: []string{"*"}, Substring: "capacity_exhausted", Kind: LimitKindQuota},
	// Agent-specific rules precede the generic "*" rules so the more
	// specific intent wins under first-match-wins (classifyLimitWithRules).
	// Codex ChatGPT-account usage cap — a quota skip, not a hard failure.
	{Agents: []string{"codex"}, Substring: "you've hit your usage limit", Kind: LimitKindQuota},
	// Transient/outage — observed provider wording only (no speculative
	// substrings; see the no-speculative note above). Retried with backoff.
	{Agents: []string{"*"}, Substring: "too many requests", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "status: 429", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "status 429", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "stream disconnected before completion", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "stream reported failure: reconnecting", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "500 internal server error", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "502 bad gateway", Kind: LimitKindTransient},
	{Agents: []string{"*"}, Substring: "503 service unavailable", Kind: LimitKindTransient},
}

// ClassifyLimit inspects an agent error message and returns a
// LimitClassification describing whether (and how) the agent is
// rate-limited. The agent argument is the canonical agent name; the
// caller is responsible for resolving any aliases (e.g. "claude" →
// "claude-code") before calling.
//
// Returns Kind == LimitKindNone when no rule matches.
func ClassifyLimit(agent, errMsg string) LimitClassification {
	return classifyLimitWithRules(agent, errMsg, defaultLimitRules)
}

// classifyLimitWithRules is ClassifyLimit with an explicit rule slice.
// Unexported; used inside the package's own tests so synthetic fixtures
// (e.g. a LimitKindSession pattern) do not leak into defaultLimitRules.
func classifyLimitWithRules(agent, errMsg string, rules []limitRule) LimitClassification {
	if errMsg == "" {
		return LimitClassification{Kind: LimitKindNone, Agent: agent, Message: errMsg}
	}
	lower := strings.ToLower(errMsg)
	for _, r := range rules {
		if !limitRuleAppliesToAgent(r, agent) {
			continue
		}
		if !strings.Contains(lower, r.Substring) {
			continue
		}
		return LimitClassification{
			Kind:        r.Kind,
			Agent:       agent,
			ResetAt:     ParseResetTime(errMsg),
			CooldownFor: ParseResetDuration(errMsg),
			Message:     errMsg,
		}
	}
	return LimitClassification{Kind: LimitKindNone, Agent: agent, Message: errMsg}
}

func limitRuleAppliesToAgent(r limitRule, agent string) bool {
	for _, a := range r.Agents {
		if a == "*" || a == agent {
			return true
		}
	}
	return false
}
