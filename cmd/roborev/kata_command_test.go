package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
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
		fmt.Fprint(w, `{"review_uuid":"synthetic","document_hash":"hash","state":"partial","expired":false,"issue_uid":"issue-uid","issue_short_id":"abc1","destination_id":"destination","roots":[{"position":0,"state":"complete","comment":{"uid":"root-uid","handle":"c:root","body":"root","author":"alice"}}],"comments":[{"uid":"reply-uid","handle":"c:reply","body":"full reply","author":"bob","teammate":"reviewer","reply_to_uid":"root-uid"}]}`)
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
		assert.Contains(t, output.String(), "full reply")
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
	for _, verb := range []string{"show", "create"} {
		root := newRootCmd()
		root.SetArgs([]string{"--server", "https://reviews.example.test", "kata", verb, "42"})
		require.ErrorContains(t, root.Execute(), "other commands require a local daemon")
	}
}
