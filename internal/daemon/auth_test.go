package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func newAuthTestServer(t *testing.T, key string) *Server {
	t.Helper()
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.AuthKey = key
	s := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestAuthProtectsAllRoutes(t *testing.T) {
	s := newAuthTestServer(t, "test-shared-key")
	mux := http.NewServeMux()
	api := s.registerHumaAPI(mux)
	paths := []string{"/api/shutdown", "/api/events", "/debug/pprof/", "/debug/pprof/cmdline", "/openapi.json", "/openapi.yaml", "/unknown"}
	for path := range api.OpenAPI().Paths {
		paths = append(paths, path)
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+path, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				r := httptest.NewRequest(method, path, strings.NewReader(`{"anything":true}`)).WithContext(ctx)
				w := httptest.NewRecorder()
				s.httpServer.Handler.ServeHTTP(w, r)
				a := assert.New(t)
				a.Equal(http.StatusUnauthorized, w.Code)
				a.Equal("Bearer", w.Header().Get("WWW-Authenticate"))
				a.Equal("application/json", w.Header().Get("Content-Type"))
				a.NotContains(w.Body.String(), "test-shared-key")
			})
		}
	}
	shutdown := false
	select {
	case <-s.shutdownCh:
		shutdown = true
	default:
	}
	assert.False(t, shutdown, "unauthorized request initiated shutdown")
}

func TestAuthBearerCredentials(t *testing.T) {
	s := newAuthTestServer(t, "test-shared-key")
	for _, tc := range []struct {
		name    string
		headers []string
		query   string
		want    int
	}{
		{"valid", []string{"Bearer test-shared-key"}, "", 200},
		{"scheme case", []string{"bEaReR test-shared-key"}, "", 200},
		{"missing", nil, "", 401},
		{"wrong key", []string{"Bearer wrong-key"}, "", 401},
		{"key case", []string{"Bearer Test-shared-key"}, "", 401},
		{"basic", []string{"Basic test-shared-key"}, "", 401},
		{"no scheme", []string{"test-shared-key"}, "", 401},
		{"empty", []string{"Bearer "}, "", 401},
		{"extra", []string{"Bearer test-shared-key extra"}, "", 401},
		{"duplicate", []string{"Bearer test-shared-key", "Bearer test-shared-key"}, "", 401},
		{"query", nil, "?auth_key=test-shared-key", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/ping"+tc.query, nil)
			for _, h := range tc.headers {
				r.Header.Add("Authorization", h)
			}
			w := httptest.NewRecorder()
			s.httpServer.Handler.ServeHTTP(w, r)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

func TestAuthChallengeProvesServerKey(t *testing.T) {
	const key = "test-shared-key"
	s := newAuthTestServer(t, key)
	r := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	const nonce = "synthetic-client-nonce"
	r.Header.Set("X-Roborev-Auth-Nonce", hex.EncodeToString([]byte(nonce)))
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, r)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	proof, err := hex.DecodeString(w.Header().Get("X-Roborev-Auth-Proof"))
	require.NoError(t, err)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("roborev-daemon-auth-v1:server:"))
	_, _ = mac.Write([]byte(nonce))
	assert.True(t, hmac.Equal(proof, mac.Sum(nil)))
}

func TestAuthDisabledAndPinnedAcrossReload(t *testing.T) {
	s := newAuthTestServer(t, "")
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	s = newAuthTestServer(t, "test-shared-key")
	s.configWatcher.cfgMu.Lock()
	s.configWatcher.cfg = config.DefaultConfig()
	s.configWatcher.cfgMu.Unlock()
	w = httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
