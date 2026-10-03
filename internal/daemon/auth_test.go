package daemon

import (
	"context"
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
	s := newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
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
				a.NotContains(w.Body.String(), "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
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
	s := newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	for _, tc := range []struct {
		name    string
		headers []string
		query   string
		want    int
	}{
		{"valid", []string{"Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"}, "", 200},
		{"scheme case", []string{"bEaReR 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"}, "", 200},
		{"missing", nil, "", 401},
		{"wrong key", []string{"Bearer wrong-key"}, "", 401},
		{"key case", []string{"Bearer Test-shared-key"}, "", 401},
		{"basic", []string{"Basic 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"}, "", 401},
		{"no scheme", []string{"51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"}, "", 401},
		{"empty", []string{"Bearer "}, "", 401},
		{"extra", []string{"Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015 extra"}, "", 401},
		{"duplicate", []string{"Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", "Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"}, "", 401},
		{"query", nil, "?auth_key=51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", 401},
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

func TestAuthDisabledAndPinnedAcrossReload(t *testing.T) {
	s := newAuthTestServer(t, "")
	w := httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	s = newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	s.configWatcher.cfgMu.Lock()
	s.configWatcher.cfg = config.DefaultConfig()
	s.configWatcher.cfgMu.Unlock()
	w = httptest.NewRecorder()
	s.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
