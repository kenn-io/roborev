package main

import (
	"bytes"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
)

func TestRemoteShowRequiresCommentsBeforeOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		json    bool
		status  int
		wantErr string
	}{
		{"text merged comments", false, http.StatusOK, ""},
		{"json merged comments", true, http.StatusOK, ""},
		{"text comments unavailable", false, http.StatusServiceUnavailable, "503 Service Unavailable"},
		{"json comments unavailable", true, http.StatusServiceUnavailable, "503 Service Unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			secret := bytes.Repeat([]byte{0x5a}, 64)
			key := requestsigning.Key{ID: "reader", Secret: secret}
			var mu sync.Mutex
			var queries []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := requestsigning.VerifyHeaders(r, "https://"+r.Host+r.URL.RequestURI(), map[string]requestsigning.Key{key.ID: key}, time.Now())
				assert.NoError(err)
				if err != nil {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/history/api/ping":
					_, _ = w.Write([]byte(`{"ok":true,"service":"roborev","version":"test"}`))
				case "/history/api/review":
					assert.Equal("42", r.URL.Query().Get("job_id"))
					_, _ = w.Write([]byte(`{"id":1,"job_id":42,"agent":"test","output":"review output","job":{"id":42,"commit_id":7,"git_ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","job_type":"review"}}`))
				case "/history/api/comments":
					mu.Lock()
					queries = append(queries, r.URL.RawQuery)
					mu.Unlock()
					if r.URL.Query().Get("job_id") != "42" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.WriteHeader(tc.status)
					if tc.status == http.StatusOK {
						_, _ = w.Write([]byte(`{"responses":[{"id":1,"job_id":42,"responder":"reader","response":"job feedback"},{"id":2,"commit_id":7,"responder":"reader","response":"legacy feedback"}]}`))
					} else {
						_, _ = w.Write([]byte(`{"error":"remote request capacity reached"}`))
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			caFile := filepath.Join(t.TempDir(), "ca.pem")
			require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
			t.Setenv("TEST_SHOW_SIGNING_SECRET", hex.EncodeToString(secret))
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "auth_key='0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'\n[remote_client]\nexternal_url='%s/history'\nkey_id='reader'\nsecret_env='TEST_SHOW_SIGNING_SECRET'\nca_file='%s'\n", server.URL, caFile), 0o600))
			oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
			serverAddr, parsedServerEndpoint = server.URL+"/history", nil
			t.Cleanup(func() { serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint })
			cmd := showCmd()
			args := []string{"--job", "42"}
			if tc.json {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var output, notices bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&notices)
			var commandErr error
			stdout, stderr := captureStdoutStderr(t, func() { commandErr = cmd.Execute() })
			mu.Lock()
			assert.Equal([]string{"job_id=42"}, queries)
			mu.Unlock()
			assert.Empty(notices.String() + stderr)
			if tc.wantErr != "" {
				assert.Empty(output.String() + stdout)
				require.ErrorContains(t, commandErr, tc.wantErr)
				return
			}
			require.NoError(t, commandErr)
			assert.Contains(output.String()+stdout, "review output")
			assert.Contains(output.String()+stdout, "job feedback")
			assert.Contains(output.String()+stdout, "legacy feedback")
		})
	}
}
