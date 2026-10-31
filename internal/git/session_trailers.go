package git

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// AgentSession is the validated identity in an Agent-Session commit line.
type AgentSession struct {
	Agent string
	ID    string
}

// CanonicalID uses agentsview's identity mapping: Claude IDs are bare.
func (s AgentSession) CanonicalID() string {
	if s.Agent == "claude" {
		return s.ID
	}
	return s.Agent + ":" + s.ID
}

var (
	agentSessionLine = regexp.MustCompile(`^Agent-Session:[ \t]*([a-z0-9][a-z0-9-]{0,31}):([A-Za-z0-9][A-Za-z0-9._-]{0,127})[ \t]*$`)
	sessionUUID      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ParseAgentSessions accepts original message lines anywhere, including squash
// bodies. It preserves discovery order and never normalizes indentation.
func ParseAgentSessions(message string) []AgentSession {
	var out []AgentSession
	seen := map[AgentSession]bool{}
	for line := range strings.SplitSeq(message, "\n") {
		m := agentSessionLine.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			continue
		}
		s := AgentSession{Agent: m[1], ID: m[2]}
		if (s.Agent == "claude" || s.Agent == "codex") && !sessionUUID.MatchString(s.ID) {
			continue
		}
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}

// GetCommitMessageCtx reads %B without folding the subject or trimming lines.
func GetCommitMessageCtx(ctx context.Context, repoPath, sha string) (string, error) {
	cmd := newGitCmdContext(ctx, "log", "-1", "--format=%B", sha)
	cmd.Dir = repoPath
	out, err := gitOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("read commit message: %w", err)
	}
	return string(out), nil
}
