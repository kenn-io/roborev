package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/pkg/client"
)

func TestKataCommandsThroughRoot(t *testing.T) {
	var method, query, body string
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
		fmt.Fprint(w, `{"review_uuid":"synthetic","document_hash":"hash","state":"partial\u001b[2J","expired":false,"issue_uid":"issue-uid\r","issue_short_id":"abc1\u001b[2J","project":"other","destination_id":"destination\u001b[2J","source_output":"Original 雪\u001b[2J finding\n\tnext\u0000","issue":{"uid":"issue-uid","issue_id":2,"project_id":7,"project":"other","short_id":"abc1","qualified_id":"other#abc1","status":"open","labels":[],"generation":1,"title":"Issue 雪\u001b[2J title\r","body":"Issue body 雪\n\tsecond\u001b[2J line\u0000"},"roots":[{"position":0,"state":"complete\u001b[2J","comment":{"uid":"root-uid\u001b[2J","handle":"c:root\u001b[2J","body":"root","author":"alice\u001b]0;owned\u0007"}}],"comments":[{"uid":"reply-uid","handle":"c:reply\u001b[2J","body":"current reply\n\tsecond line with enough evidence for confirmation\u001b[2J\u0000","author":"bob\u001b[2J","teammate":"reviewer\u001b[2J","reply_to_uid":"root-uid\u001b[2J","reply_kind":"confirm","lineage":"complete\u001b[2J","edited_at":"2026-01-02T03:04:05Z"},{"uid":"unedited-uid","body":"original reply","author":"eve","reply_to_uid":"root-uid"},{"uid":"root-comment","issue_id":2,"body":"root 雪\n\ttext\u001b[2J\u0000","author":"author 雪\u001b[2J"}]}`)
	}))
	defer server.Close()
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	for _, tc := range []struct {
		args   []string
		method string
	}{{[]string{"kata", "show", "42"}, http.MethodGet}, {[]string{"kata", "create", "42", "--findings", "comments"}, http.MethodPost}} {
		root := newRootCmd()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(append([]string{"--server", server.URL}, tc.args...))
		require.NoError(t, root.Execute())
		assert.Equal(t, tc.method, method)
		assert.Contains(t, output.String(), "Status: partial")
		assert.Contains(t, output.String(), "Finding 1: complete c:root")
		assert.Contains(t, output.String(), "Project: other\n")
		assert.Contains(t, output.String(), "Issue: abc1 (issue-uid)")
		assert.Contains(t, output.String(), "Destination: destination")
		assert.Contains(t, output.String(), "(author: alice; UID: root-uid)")
		assert.Contains(t, output.String(), "Original review:\nOriginal 雪 finding\n\tnext")
		assert.Contains(t, output.String(), "Issue: Issue 雪 title\nIssue body 雪\n\tsecond line")
		assert.Contains(t, output.String(), "Reply c:reply (reply-uid; confirm) by bob to root-uid (reviewer); lineage: complete [edited]; edit history: unknown:\ncurrent reply\n\tsecond line")
		assert.Contains(t, output.String(), "by eve to root-uid; edit history: unknown:\noriginal reply\n")
		assert.Contains(t, output.String(), "by author 雪; edit history: unknown:\nroot 雪\n\ttext")
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

func TestKataShowSelectorQueries(t *testing.T) {
	var query string
	var authenticationFailure bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ping" {
			fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"test"}`)
			return
		}
		query = r.URL.RawQuery
		if authenticationFailure {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"status":401,"detail":"authentication\u001b[2J required"}`)
			return
		}
		fmt.Fprint(w, `{"review_uuid":"","document_hash":"","state":"unverified","expired":false,"roots":[],"comments":[{"uid":"c1","body":"evidence","author":"actual","lineage":"orphan","history":"unknown"}]}`)
	}))
	defer server.Close()
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
	for _, tc := range []struct {
		selector, param string
		issue           bool
	}{
		{"11111111-1111-4111-8111-111111111111", "review_uuid=", false},
		{"issue-uid", "issue=issue-uid", false},
		{"reviews#1234", "issue=reviews%231234", false},
		{"1234", "job_id=1234", false},
		{"1234", "issue=1234", true},
	} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		args := []string{"--server", server.URL, "kata", "show", tc.selector, "--project", "reviews"}
		if tc.issue {
			args = append(args, "--issue")
		}
		root.SetArgs(args)
		require.NoError(t, root.Execute())
		assert.Contains(t, query, tc.param)
		if tc.issue {
			assert.NotContains(t, query, "job_id=")
		}
		assert.Contains(t, query, "project=reviews")
		assert.Contains(t, out.String(), "evidence")
		assert.Contains(t, out.String(), "lineage: orphan")
	}
	authenticationFailure = true
	root := newRootCmd()
	root.SetArgs([]string{"--server", server.URL, "kata", "show", "42"})
	err := root.Execute()
	require.ErrorIs(t, err, client.ErrKataAuthentication)
	assert.NotContains(t, err.Error(), "\x1b")
	assert.Contains(t, err.Error(), "authentication required")
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
	text := "Finding 1 from a roborev review."
	digest := sha256.Sum256([]byte(text))
	rootBody := fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", text, review, digest)
	rootUID := "01AAAAAAAAAAAAAAAAAAABC123"
	var posts atomic.Int64
	var posted, selectedProject, producerError, prePostOutput atomic.Value
	var output bytes.Buffer
	producerError.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ping":
			fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"test"}`)
		case "/api/kata/publication":
			fmt.Fprintf(w, `{"review_uuid":%q,"document_hash":%q,"state":"complete","expired":false,"issue_uid":"issue-uid","issue_short_id":"abc1","project":"other","destination_id":"destination","roots":[{"position":0,"state":"complete","comment":{"uid":%q,"body":"root","author":"service"}}],"comments":[]}`, review, strings.Repeat("a", 64), rootUID)
		case "/api/kata/issue":
			selectedProject.Store(r.URL.Query().Get("project"))
			_ = json.NewEncoder(w).Encode(map[string]any{"uid": "issue-uid", "issue_id": 2, "project_id": 7, "project": "other", "project_uid": "project-uid", "destination_id": "destination", "reply_teammate": "roborev", "generation": 3, "short_id": "abc1", "qualified_id": "other#abc1", "title": "issue", "body": "body", "status": "open", "labels": []string{}, "comments_complete": true, "metadata": map[string]any{"roborev.version": 1, "roborev.review_uuid": review}, "publication_envelope": map[string]any{"version": 1, "review_uuid": review, "document_hash": strings.Repeat("a", 64), "policy": "summary", "completed_at": "2026-01-02T03:04:05Z", "finding_count": 1}, "comments": []any{map[string]any{"uid": rootUID, "handle": "c:abc123", "issue_id": 2, "body": rootBody, "author": "service"}}})
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
	assert.Contains(t, shown.String(), "Issue: abc1 (issue-uid)\n")
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
	retryArgs, err := shlex.Split(strings.TrimSuffix(retry, "\n"))
	require.NoError(t, err)
	assert.Equal(t, []string{"roborev", "--server", strings.TrimPrefix(server.URL, "http://"), "kata", "comment", "issue-uid", "--project", "other", "--destination-id", "destination", "--expected-teammate", "roborev", "--reply", rootUID, "--idempotency-key", key, "-m", body}, retryArgs)
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
		assert.ErrorContains(t, err, "synthetic producer error")
	}
}
