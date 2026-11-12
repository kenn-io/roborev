package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestKataCommandsThroughRoot(t *testing.T) {
	var method, query, body, mode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ping" {
			fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"test"}`)
			return
		}
		method, query = r.Method, r.URL.RawQuery
		if r.Method == http.MethodPost {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			body = buf.String()
		}
		response := `{"project":"canonical\u001b[2J","review_uuid":"synthetic","document_hash":"hash","state":"partial\u001b[2J","expired":false,"issue_uid":"issue-uid\r","issue_short_id":"abc1\u001b[2J","destination_id":"destination\u001b[2J","roots":[{"position":0,"state":"complete\u001b[2J","comment":{"uid":"root-uid\u001b[2J","handle":"c:root\u001b[2J","body":"root","author":"alice\u001b]0;owned\u0007"}}],"comments":[{"uid":"reply-uid","reply_kind":"confirm\u001b[2J","edited_at":"2026-01-01T00:00:00Z","handle":"c:reply\u001b[2J","body":"full reply\n\tsecond line\u001b[2J\u0000","author":"bob\u001b[2J","teammate":"reviewer\u001b[2J","reply_to_uid":"root-uid\u001b[2J"},{"uid":"reply-2","handle":"c:two","body":"disagree","author":"eve","reply_kind":"refute","reply_to_uid":"root-uid"},{"uid":"reply-3","handle":"c:three","body":"plain","author":"sam","reply_to_uid":"root-uid"}]}`
		if r.Method == http.MethodPost {
			response = strings.Replace(response, `canonical\u001b[2J`, `other-project\u001b[2J`, 1)
		}
		switch mode {
		case "legacy":
			response = strings.Replace(response, `"project":"canonical\u001b[2J",`, "", 1)
		case "no-short":
			response = strings.Replace(response, `"issue_short_id":"abc1\u001b[2J",`, "", 1)
		}
		fmt.Fprint(w, response)
	}))
	defer server.Close()
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	for _, tc := range []struct {
		args    []string
		method  string
		mode    string
		issue   string
		project string
	}{
		{[]string{"kata", "show", "42"}, http.MethodGet, "", "canonical#abc1 (issue-uid)", "canonical"},
		{[]string{"kata", "create", "42", "--findings", "comments"}, http.MethodPost, "", "other-project#abc1 (issue-uid)", "other-project"},
		{[]string{"kata", "show", "42"}, http.MethodGet, "legacy", "abc1 (issue-uid)", ""},
		{[]string{"kata", "show", "42"}, http.MethodGet, "no-short", "issue-uid", "canonical"},
	} {
		mode = tc.mode
		root := newRootCmd()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(append([]string{"--server", server.URL}, tc.args...))
		require.NoError(t, root.Execute())
		assert.Equal(t, tc.method, method)
		assert.Contains(t, output.String(), "Status: partial")
		assert.Contains(t, output.String(), "Finding 1: complete c:root")
		assert.Contains(t, output.String(), "Issue: "+tc.issue+"\n")
		if tc.project != "" {
			assert.Contains(t, output.String(), "Project: "+tc.project+"\n")
			assert.Equal(t, 1, strings.Count(output.String(), "Project: "))
		} else {
			assert.NotContains(t, output.String(), "Project:")
		}
		assert.Contains(t, output.String(), "Destination: destination")
		assert.Contains(t, output.String(), "(author: alice; UID: root-uid)")
		assert.Contains(t, output.String(), "Reply c:reply (reply-uid; confirm) by bob to root-uid (reviewer) [edited]:\nfull reply\n\tsecond line")
		assert.Contains(t, output.String(), "Reply c:two (reply-2; refute) by eve to root-uid:\ndisagree")
		assert.Contains(t, output.String(), "Reply c:three (reply-3) by sam to root-uid:\nplain")
		assert.NotContains(t, output.String(), "\x1b")
		assert.NotContains(t, output.String(), "\r")
		assert.NotContains(t, output.String(), "\x00")
		if tc.method == http.MethodGet {
			assert.Contains(t, query, "job_id=42")
		} else {
			assert.Contains(t, body, `"findings":"comments"`)
		}
	}
}

func TestKataRemoteCommandsRejectedThroughRoot(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.test'\n"), 0o600))
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	for _, verb := range []string{"show", "create", "comment"} {
		root := newRootCmd()
		root.SetArgs([]string{"--server", "https://reviews.example.test", "kata", verb, "42"})
		require.ErrorContains(t, root.Execute(), "other commands require a local daemon")
	}
}

type shortKataWriter struct{}

func (shortKataWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

type failingKataWriter struct{}

func (failingKataWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestKataCommentPrintsRetryBeforePosting(t *testing.T) {
	review := "11111111-1111-4111-8111-111111111111"
	rootUID := "01AAAAAAAAAAAAAAAAAAABC123"
	var posts atomic.Int64
	var posted, selectedProject, producerError, prePostOutput, commentUIDs atomic.Value
	var commentsComplete atomic.Bool
	var output bytes.Buffer
	producerError.Store("")
	commentUIDs.Store([]string{rootUID})
	commentsComplete.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ping":
			fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"test"}`)
		case "/api/kata/publication":
			fmt.Fprintf(w, `{"review_uuid":%q,"document_hash":%q,"state":"complete","expired":false,"issue_uid":"issue-uid","issue_short_id":"abc1","project":"other","destination_id":"destination","roots":[{"position":0,"state":"complete","comment":{"uid":%q,"body":"root","author":"service"}}],"comments":[]}`, review, strings.Repeat("a", 64), rootUID)
		case "/api/kata/issue":
			selectedProject.Store(r.URL.Query().Get("project"))
			var comments []any
			for _, uid := range commentUIDs.Load().([]string) {
				comments = append(comments, map[string]any{"uid": uid, "body": "body", "author": "service"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"uid": "issue-uid", "project": "other", "destination_id": "destination", "reply_teammate": "roborev", "generation": 3, "short_id": "abc1", "qualified_id": "other#abc1", "title": "issue", "body": "body", "status": "open", "labels": []string{}, "comments_complete": commentsComplete.Load(), "comments": comments})
		case "/api/kata/response":
			prePostOutput.Store(output.String())
			posts.Add(1)
			var data map[string]any
			_ = json.NewDecoder(r.Body).Decode(&data)
			posted.Store(data)
			if code := producerError.Load().(string); code != "" {
				status := http.StatusConflict
				switch code {
				case "validation":
					status = http.StatusBadRequest
				case "comment_not_found":
					status = http.StatusNotFound
				}
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"status":%d,"detail":%q}`, status, "Kata "+code+": synthetic producer error")
				return
			}
			fmt.Fprint(w, `{"uid":"reply-uid","author":"actual\u001b[2J-author","teammate":"roborev","changed":false,"body":"edited\u001b[2J evidence\r\n\tsecond\u0000 line","edited_at":"2026-01-02T03:04:05Z"}`)
		}
	}))
	defer server.Close()
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	show := newRootCmd()
	var shown bytes.Buffer
	show.SetOut(&shown)
	show.SetErr(&shown)
	show.SetArgs([]string{"--server", server.URL, "kata", "show", "42"})
	require.NoError(t, show.Execute())
	assert.Contains(t, shown.String(), "Issue: other#abc1 (issue-uid)\n")
	assert.Contains(t, shown.String(), "UID: "+rootUID)
	fields := make(map[string]string)
	for line := range strings.SplitSeq(shown.String(), "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			fields[key] = value
		}
	}
	require.Equal(t, "other", fields["Project"])
	require.Equal(t, "destination", fields["Destination"])
	key, body := "Case-'Key", "It's evidence\n\twith 'quotes'"
	args := []string{"--server", server.URL, "kata", "comment", "issue-uid", "--project", fields["Project"], "--destination-id", fields["Destination"], "--reply", rootUID, "--idempotency-key", key, "-m", body}
	staleArgs := append([]string(nil), args...)
	staleArgs[8] = "changed-destination"
	stale := newRootCmd()
	stale.SetOut(&output)
	stale.SetErr(&output)
	stale.SetArgs(staleArgs)
	require.ErrorContains(t, stale.Execute(), "destination changed")
	assert.Zero(t, posts.Load())
	changedTeammate := newRootCmd()
	changedTeammate.SetOut(&output)
	changedTeammate.SetErr(&output)
	changedTeammate.SetArgs(append(append([]string(nil), args...), "--expected-teammate", "old-teammate"))
	require.ErrorContains(t, changedTeammate.Execute(), "teammate changed")
	assert.Zero(t, posts.Load())
	for _, writer := range []io.Writer{shortKataWriter{}, failingKataWriter{}} {
		root := newRootCmd()
		root.SetOut(writer)
		root.SetErr(&output)
		root.SetArgs(args)
		require.ErrorContains(t, root.Execute(), "write retry instructions")
		assert.Zero(t, posts.Load())
	}
	output.Reset()
	root := newRootCmd()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	require.NoError(t, root.Execute())
	assert.Equal(t, int64(1), posts.Load())
	assert.Equal(t, "other", selectedProject.Load())
	assert.Contains(t, output.String(), "Retry: roborev --server '"+strings.TrimPrefix(server.URL, "http://")+"'")
	assert.Contains(t, output.String(), "actual-author")
	assert.Contains(t, output.String(), "--expected-teammate ")
	assert.Contains(t, output.String(), "Current body (edited):\nedited evidence\n\tsecond line")
	assert.NotContains(t, output.String(), "\x1b")
	assert.NotContains(t, output.String(), "\r")
	assert.NotContains(t, output.String(), "\x00")
	instructions, ok := prePostOutput.Load().(string)
	require.True(t, ok)
	_, retry, ok := strings.Cut(instructions, "Retry: ")
	require.True(t, ok)
	if runtime.GOOS == "windows" {
		// ShellEscape uses PowerShell single quotes, whose doubled apostrophes
		// cannot be parsed with POSIX shlex rules.
		want := fmt.Sprintf("roborev --server '%s' kata comment 'issue-uid' --project 'other' --destination-id 'destination' --expected-teammate 'roborev' --reply '%s' --idempotency-key 'Case-''Key' -m 'It''s evidence\n\twith ''quotes'''\n", strings.TrimPrefix(server.URL, "http://"), rootUID)
		assert.Equal(t, want, retry)
	} else {
		retryArgs, err := shlex.Split(strings.TrimSuffix(retry, "\n"))
		require.NoError(t, err)
		assert.Equal(t, []string{"roborev", "--server", strings.TrimPrefix(server.URL, "http://"), "kata", "comment", "issue-uid", "--project", "other", "--destination-id", "destination", "--expected-teammate", "roborev", "--reply", rootUID, "--idempotency-key", key, "-m", body}, retryArgs)
	}
	payload, ok := posted.Load().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, rootUID, payload["target_uid"])
	assert.Equal(t, "other", payload["project"])
	assert.Equal(t, "destination", payload["destination_id"])
	assert.Equal(t, body, payload["body"])
	assert.Equal(t, key, payload["idempotency_key"])
	assert.Equal(t, "roborev", payload["teammate"])
	for _, code := range []string{"validation", "comment_not_found", "duplicate_reply", "idempotency_mismatch", "issue_closed"} {
		producerError.Store(code)
		root := newRootCmd()
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		err := root.Execute()
		require.ErrorContains(t, err, code)
		require.ErrorContains(t, err, "synthetic producer error")
	}
	producerError.Store("")
	for _, tc := range []struct {
		name                  string
		uids                  []string
		target, want, problem string
		complete              bool
	}{
		{"full mixed case", []string{rootUID}, strings.ToLower(rootUID), rootUID, "", true},
		{"suffix mixed case", []string{rootUID}, "C:aBc123", rootUID, "", true},
		{"missing", []string{rootUID}, "c:missing", "", "not found", true},
		{"ambiguous", []string{rootUID, "01BBBBBBBBBBBBBBBBBBABC123"}, "c:abc123", "", "ambiguous", true},
		{"duplicate", []string{rootUID, strings.ToLower(rootUID)}, rootUID, "", "duplicate", true},
		{"empty uid", []string{rootUID, ""}, rootUID, "", "missing UID", true},
		{"incomplete", []string{rootUID}, rootUID, "", "complete publication", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commentUIDs.Store(tc.uids)
			commentsComplete.Store(tc.complete)
			before := posts.Load()
			output.Reset()
			invocation := append([]string(nil), args...)
			invocation[10] = tc.target
			cmd := newRootCmd()
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(invocation)
			err := cmd.Execute()
			if tc.problem != "" {
				require.ErrorContains(t, err, tc.problem)
				assert.Equal(t, before, posts.Load())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, before+1, posts.Load())
			assert.Equal(t, tc.want, posted.Load().(map[string]any)["target_uid"])
			assert.Contains(t, prePostOutput.Load().(string), "Target: "+tc.want+"\n")
			assert.Contains(t, prePostOutput.Load().(string), "--reply '"+tc.want+"' ")
		})
	}
}
