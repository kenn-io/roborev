package agent

import (
	"bytes"
	"encoding/json/jsontext"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiReviewMissingSessionFailsBeforeLaunch(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	a := NewPiAgent("nonexistent-test-agent").WithSessionID("missing-session").(*PiAgent)
	_, err := a.Review(t.Context(), t.TempDir(), "HEAD", "prompt", nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "session not found")
}

func TestPiReviewWithSchemaMissingSessionFailsBeforeLaunch(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	a := NewPiAgent("nonexistent-test-agent").WithSessionID("missing-session").(*PiAgent)
	_, err := a.ReviewWithSchema(t.Context(), t.TempDir(), "HEAD", "prompt", jsontext.Value(`{}`), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "session not found")
}

func TestPiStructuredReviewCreatesAndResumesSession(t *testing.T) {
	skipIfWindows(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dataDir)
	const sessionID = "46109439-3160-40f0-81e7-7dfa4f3647b3"
	script := `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
session=""
mode=""
no_session=""
output=""
prev=""
for arg in "$@"; do
 case "$prev" in
 --session) session="$arg" ;;
 --mode) mode="$arg" ;;
 --json-output) output="$arg" ;;
 esac
 if [ "$arg" = "--no-session" ]; then no_session=yes; fi
 prev="$arg"
done
if [ -n "$session" ]; then
 test -f "$session" || exit 2
 result='{"review":"resumed"}'
else
 session="$PI_CODING_AGENT_DIR/sessions/--repo--/review_46109439-3160-40f0-81e7-7dfa4f3647b3.jsonl"
 result='{"review":"fresh"}'
fi
if [ -z "$no_session" ]; then
 mkdir -p "$(dirname "$session")"
 printf '%s\n' '{"type":"message"}' >> "$session"
fi
if [ "$mode" = json ]; then
 printf '%s\n' '{"type":"session","id":"46109439-3160-40f0-81e7-7dfa4f3647b3"}'
fi
printf '%s\n' "$result" > "$output"
`
	a := NewPiAgent(writeTempCommand(t, script))
	repoDir := t.TempDir()
	capture := NewSessionCaptureWriter(&bytes.Buffer{}, nil)
	result, err := a.ReviewWithSchema(t.Context(), repoDir, "HEAD", "original review", jsontext.Value(`{}`), capture)
	require.NoError(t, err)
	assert.JSONEq(`{"review":"fresh"}`, string(result))
	assert.Equal(sessionID, capture.SessionID())
	sessionPath := filepath.Join(dataDir, "sessions", "--repo--", "review_"+sessionID+".jsonl")
	require.FileExists(t, sessionPath)
	resumed := a.WithSessionID(capture.SessionID()).(*PiAgent)
	resumeCapture := NewSessionCaptureWriter(&bytes.Buffer{}, nil)
	result, err = resumed.ReviewWithSchema(t.Context(), repoDir, "HEAD", "focused follow-up", jsontext.Value(`{}`), resumeCapture)
	require.NoError(t, err)
	assert.JSONEq(`{"review":"resumed"}`, string(result))
	assert.Equal(sessionID, resumeCapture.SessionID())
	lines := readLineFile(t, sessionPath)
	require.Len(t, lines, 2)
}
