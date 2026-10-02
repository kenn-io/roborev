package daemon

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestAuthBrowserRequiresLoginAndAllowsAuthenticatedAPI(t *testing.T) {
	for _, tc := range []struct {
		name, browserToken, loginToken, wrongToken, publicOrigin string
		local, forwarded                                         bool
	}{
		{"shared key", "", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", "", true, false},
		{"explicit browser token", testBrowserAuthToken, testBrowserAuthToken, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "", false, false},
		{"forwarded shared key", "", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", "", false, true},
		{"public origin shared key", "", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210", "https://reviews.example.com", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := assert.New(t)
			configPath := filepath.Join(t.TempDir(), "config.toml")
			contents := fmt.Sprintf("auth_key = \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n[web]\nlisten = \"127.0.0.1:0\"\nauth_token = %q\npublic_origin = %q\n", tc.browserToken, tc.publicOrigin)
			require.NoError(t, os.WriteFile(configPath, []byte(contents), 0o600))
			cfg, err := config.LoadGlobalFrom(configPath)
			require.NoError(t, err)
			s := newAuthTestServer(t, cfg.AuthKey)
			marker := filepath.Join(t.TempDir(), "comment-hook")
			s.configWatcher.Config().Hooks = []config.HookConfig{{Event: "review.commented", Command: touchCmd(marker)}}
			s.allowWebCompilationStub = true
			runtime, err := s.startBrowserServer(cfg.Web)
			require.NoError(t, err)
			base := "http://" + runtime.Address
			origin, err := url.Parse(runtime.Origin)
			require.NoError(t, err)
			request := func(path, body string) *http.Response {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewBufferString(body))
				require.NoError(t, err)
				req.Host = origin.Host
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", runtime.Origin)
				req.Header.Set("Sec-Fetch-Site", "same-origin")
				req.Header.Set("Sec-Fetch-Mode", "cors")
				req.Header.Set("Sec-Fetch-Dest", "empty")
				if tc.forwarded {
					req.Header.Set("X-Forwarded-For", "192.0.2.1")
				}
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				t.Cleanup(func() { _ = resp.Body.Close() })
				return resp
			}
			bootstrap := request("/api/ui/session/bootstrap", "{}")
			a.Equal(http.StatusUnauthorized, bootstrap.StatusCode)
			wrong := request("/api/ui/session/login", `{"token":"`+tc.wrongToken+`"}`)
			a.Equal(http.StatusUnauthorized, wrong.StatusCode)
			login := request("/api/ui/session/login", `{"token":"`+tc.loginToken+`"}`)
			require.Equal(t, http.StatusOK, login.StatusCode)
			var credentials WebSessionCredentials
			require.NoError(t, json.UnmarshalRead(login.Body, &credentials))
			a.Equal(WebSessionCapabilities{CancelAnyJob: tc.local, CancelReviewJob: true, RerunJob: tc.local}, credentials.Capabilities)
			for _, path := range []string{"/api/status", "/api/ping"} {
				req, err := http.NewRequest(http.MethodGet, base+path, nil)
				require.NoError(t, err)
				req.Host = origin.Host
				require.Len(t, login.Cookies(), 1)
				req.AddCookie(login.Cookies()[0])
				req.Header.Set(WebSessionHeader, credentials.Session)
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				resp.Body.Close()
				a.Equal(http.StatusOK, resp.StatusCode, path)
			}
			job := createTestJob(t, s.db, t.TempDir(), "abc123", "test")
			_, events := s.broadcaster.Subscribe("")
			body, err := json.Marshal(AddCommentRequest{JobID: job.ID, Commenter: "reviewer", Comment: "Review note"})
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodPost, base+"/api/comment", bytes.NewReader(body))
			require.NoError(t, err)
			req.Host = origin.Host
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", runtime.Origin)
			req.AddCookie(login.Cookies()[0])
			req.Header.Set(WebSessionHeader, credentials.Session)
			req.Header.Set(WebCSRFHeader, credentials.CSRF)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			var source string
			require.NoError(t, s.db.QueryRow(`SELECT source FROM responses WHERE job_id = ?`, job.ID).Scan(&source))
			wantSource := storage.ResponseSourceRemoteBrowser
			if tc.local {
				wantSource = storage.ResponseSourceLocal
			}
			a.Equal(wantSource, source)
			s.hookRunner.WaitUntilIdle()
			if tc.local {
				a.FileExists(marker)
			} else {
				a.NoFileExists(marker)
			}
			var event Event
			received := false
			select {
			case event = <-events:
				received = true
			case <-time.After(5 * time.Second):
			}
			require.True(t, received, "comment event was not broadcast")
			a.Equal(!tc.local, event.SuppressHooks)
		})
	}
}

func TestAuthProxyBrowserRequiresSharedKeyForSession(t *testing.T) {
	a := assert.New(t)
	s := newAuthTestServer(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	s.allowWebCompilationStub = true
	cfg := config.DefaultConfig()
	cfg.Web.Listen = "127.0.0.1:0"
	cfg.Web.PublicOrigin = "https://reviews.example.com"
	cfg.Web.AuthMode = config.WebAuthModeProxy
	runtime, err := s.startBrowserServer(cfg.Web)
	require.NoError(t, err)
	base := "http://" + runtime.Address
	bootstrap := func(key string) *http.Response {
		t.Helper()
		req := proxyBrowserRequest(http.MethodPost, "/api/ui/session/bootstrap", map[string]any{})
		req.URL.Scheme = "http"
		req.URL.Host = runtime.Address
		req.RequestURI = ""
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	for _, key := range []string{"", "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"} {
		resp := bootstrap(key)
		a.Equal(http.StatusUnauthorized, resp.StatusCode)
		a.Empty(resp.Cookies())
	}
	login := bootstrap("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.Equal(t, http.StatusOK, login.StatusCode)
	var credentials WebSessionCredentials
	require.NoError(t, json.UnmarshalRead(login.Body, &credentials))
	req, err := http.NewRequest(http.MethodGet, base+"/api/status", nil)
	require.NoError(t, err)
	req.Host = "reviews.example.com"
	require.Len(t, login.Cookies(), 1)
	req.AddCookie(login.Cookies()[0])
	req.Header.Set(WebSessionHeader, credentials.Session)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	a.Equal(http.StatusOK, resp.StatusCode)
}
