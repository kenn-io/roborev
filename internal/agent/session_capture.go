package agent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// ExtractSessionID returns the agent session/thread identifier carried by a
// streamed JSONL event. For Codex, thread_id is treated as the session ID.
//
// Accepts a single JSONL line, a partial line (no trailing newline), or a
// multi-line stream. Multi-line input is scanned newline by newline and
// the first line that yields a session ID wins.
func ExtractSessionID(line string) string {
	if strings.ContainsRune(line, '\n') {
		for l := range strings.SplitSeq(line, "\n") {
			if id := extractSessionIDFromLine(l); id != "" {
				return id
			}
		}
		return ""
	}
	return extractSessionIDFromLine(line)
}

func extractSessionIDFromLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return ""
	}

	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return ""
	}

	if sessionID := jsonFieldString(fields, "session_id"); sessionID != "" {
		return sessionID
	}
	if sessionID := jsonFieldString(fields, "sessionId"); sessionID != "" {
		return sessionID
	}
	if sessionID := jsonFieldString(fields, "sessionID"); sessionID != "" {
		return sessionID
	}

	if jsonFieldString(fields, "type") == "session" {
		if sessionID := jsonFieldString(fields, "id"); sessionID != "" {
			return sessionID
		}
	}

	if jsonFieldString(fields, "type") == "thread.started" {
		if threadID := jsonFieldString(fields, "thread_id"); threadID != "" {
			return threadID
		}
		if threadID := jsonFieldString(fields, "threadId"); threadID != "" {
			return threadID
		}
	}

	return ""
}

func jsonFieldString(fields map[string]jsontext.Value, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
