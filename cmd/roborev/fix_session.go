package main

import (
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
)

// fixSessionTracker keeps the most-recent agent session ID across
// calls within a single roborev fix run and produces the agent to
// invoke next. See spec for full contract.
type fixSessionTracker struct {
	enabled bool        // --resume passed
	base    agent.Agent // resolved once (already WithAgentic/WithReasoning/WithModel)
	last    string      // most recent non-empty captured session ID
	warned  bool        // emit "agent X doesn't support resume" only once
	quiet   bool        // suppress warnings under --quiet
	out     io.Writer   // where to write the unsupported-agent warning
}

// NextAgent returns the agent to use for the next invocation along
// with a resuming flag indicating whether this call is actually
// resumed. The flag is the source of truth for "Resuming session ..."
// — relying on agent identity equality is brittle.
//
// Decision order (kept exact for contract clarity):
//  1. !enabled                        → (base, false)
//  2. !SessionAgent                   → warn once (respect quiet); (base, false)
//  3. last == ""                      → (base, false)
//  4. otherwise                       → (sa.WithSessionID(last), true)
func (t *fixSessionTracker) NextAgent() (agent.Agent, bool) {
	if !t.enabled {
		return t.base, false
	}
	sa, ok := t.base.(agent.SessionAgent)
	if !ok {
		if !t.warned {
			t.warned = true
			if !t.quiet && t.out != nil {
				fmt.Fprintf(t.out,
					"Warning: agent %s does not support session resume; running fresh sessions\n",
					t.base.Name(),
				)
			}
		}
		return t.base, false
	}
	if t.last == "" {
		return t.base, false
	}
	return sa.WithSessionID(t.last), true
}

// Capture stores a non-empty provider session ID.
func (t *fixSessionTracker) Capture(id string) {
	if id == "" {
		return
	}
	t.last = id
}

// Reset clears the tracked session ID. Called on any agent error so a
// stale session token does not poison every subsequent batch.
func (t *fixSessionTracker) Reset() {
	t.last = ""
}

// shortSessionID returns the first 12 characters of id for log lines.
func shortSessionID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// ensureBaseAgent lazily resolves the fix agent and stores it on the
// tracker. Called by entry functions just before they need an agent so
// no-op paths (no eligible jobs, all jobs already passing) don't fail
// on agent resolution that would never be exercised.
func ensureBaseAgent(repoRoot string, opts fixOptions, tracker *fixSessionTracker) error {
	if tracker.base != nil {
		return nil
	}
	base, err := resolveFixAgent(repoRoot, opts)
	if err != nil {
		return err
	}
	tracker.base = base
	return nil
}
