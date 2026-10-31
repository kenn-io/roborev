// Package provenance captures evidence connecting reviewed commits to agent
// sessions. Provider records are untrusted context, never proof of authorship.
package provenance

import (
	"context"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
)

type Turn struct {
	Ordinal int    `json:"ordinal"`
	At      string `json:"at,omitempty"`
	Prompt  string `json:"prompt"`
	Why     string `json:"why"`
}

type Identity struct {
	ID string `json:"id"`
}

// Link is the display-safe subset of the shared CommitSessionLink wire schema.
// Archive paths, project names and transcript bodies are deliberately not read.
type Link struct {
	SessionID      string    `json:"session_id"`
	CommitSHA      string    `json:"commit_sha"`
	Resolved       bool      `json:"resolved"`
	Source         string    `json:"source"`
	MessageOrdinal *int      `json:"message_ordinal"`
	CallIndex      *int      `json:"call_index"`
	Ambiguous      bool      `json:"ambiguous"`
	Agent          string    `json:"agent"`
	WebURL         string    `json:"web_url"`
	Session        *Identity `json:"session"`
	Gone           bool      `json:"gone"`
	Turn           *Turn     `json:"turn"`
}

// Session retains each distinct evidence row, including its own uncertainty.
type Session struct {
	Agent          string `json:"agent"`
	SessionID      string `json:"session_id"`
	Source         string `json:"source"`
	CommitSHA      string `json:"commit_sha"`
	Resolved       bool   `json:"resolved"`
	Ambiguous      bool   `json:"ambiguous,omitempty"`
	Availability   string `json:"availability"`
	MessageOrdinal *int   `json:"message_ordinal,omitempty"`
	CallIndex      *int   `json:"call_index,omitempty"`
	WebURL         string `json:"web_url,omitempty"`
	Turn           *Turn  `json:"turn,omitempty"`
}

type Commit struct {
	CommitSHA string    `json:"commit_sha"`
	Coverage  string    `json:"coverage"`
	Sessions  []Session `json:"sessions"`
}

type Input struct{ SHA, Message string }

type LookupResult struct {
	Commits  map[string][]Link
	Coverage map[string]string
}

type Lookup interface {
	Lookup(context.Context, string, []string, *config.Config) (LookupResult, error)
}

// Resolve combines all local trailers and returned evidence without upgrading
// ambiguous prefix matches or merging different source/turn records.
func Resolve(inputs []Input, result LookupResult) []Commit {
	var out []Commit
	for _, input := range inputs {
		c := Commit{CommitSHA: input.SHA, Coverage: "trailers", Sessions: []Session{}}
		if coverage := result.Coverage[input.SHA]; coverage != "" {
			c.Coverage = coverage
		}
		seen := map[string]bool{}
		add := func(s Session) {
			key, _ := json.Marshal(s)
			if !seen[string(key)] {
				c.Sessions = append(c.Sessions, s)
				seen[string(key)] = true
			}
		}
		for _, identity := range git.ParseAgentSessions(input.Message) {
			add(Session{Agent: identity.Agent, SessionID: identity.CanonicalID(), Source: "trailer", CommitSHA: input.SHA, Resolved: true, Availability: "unknown"})
		}
		for _, link := range result.Commits[input.SHA] {
			agent, ok := sessionAgent(link.SessionID)
			if !ok || (link.Agent != "" && link.Agent != agent) || (link.Source != "trailer" && link.Source != "transcript") {
				continue
			}
			sha := strings.ToLower(link.CommitSHA)
			requested := strings.ToLower(input.SHA)
			if !validSHA(sha) || !validSHA(requested) {
				continue
			}
			if link.Resolved {
				if len(sha) != 40 || !strings.HasPrefix(sha, requested) {
					continue
				}
			} else if !strings.HasPrefix(requested, sha) && !strings.HasPrefix(sha, requested) {
				continue
			}
			available := "gone"
			if link.Session != nil {
				if link.Gone || link.Session.ID != link.SessionID {
					continue
				}
				available = "available"
			} else if !link.Gone {
				continue
			}
			s := Session{Agent: agent, SessionID: link.SessionID, Source: link.Source, CommitSHA: sha, Resolved: link.Resolved, Ambiguous: link.Ambiguous || !link.Resolved, Availability: available, MessageOrdinal: link.MessageOrdinal, CallIndex: link.CallIndex}
			if available == "available" {
				s.WebURL = link.WebURL
				s.Turn = link.Turn
			}
			add(s)
		}
		out = append(out, c)
	}
	return out
}

func sessionAgent(id string) (string, bool) {
	agent, raw, found := strings.Cut(id, ":")
	if !found {
		agent, raw = "claude", id
	}
	parsed := git.ParseAgentSessions("Agent-Session: " + agent + ":" + raw)
	return agent, len(parsed) == 1 && parsed[0].CanonicalID() == id
}
