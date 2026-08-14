package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerServesNavigationAndAssets(t *testing.T) {
	handler, err := NewHandler(completeDistribution())
	require.NoError(t, err)

	tests := []struct {
		name        string
		method      string
		path        string
		accept      string
		status      int
		contentType string
		cache       string
	}{
		{name: "root", method: http.MethodGet, path: "/", status: http.StatusTemporaryRedirect, cache: "no-store"},
		{name: "review deep link", method: http.MethodGet, path: "/reviews/42", accept: "text/html", status: http.StatusOK, contentType: "text/html; charset=utf-8", cache: "no-store"},
		{name: "analytics", method: http.MethodGet, path: "/analytics", accept: "text/html", status: http.StatusOK, contentType: "text/html; charset=utf-8", cache: "no-store"},
		{name: "javascript", method: http.MethodGet, path: "/assets/index-a1b2c3.js", status: http.StatusOK, contentType: "text/javascript; charset=utf-8", cache: "public, max-age=31536000, immutable"},
		{name: "head", method: http.MethodHead, path: "/assets/index-a1b2c3.css", status: http.StatusOK, contentType: "text/css; charset=utf-8", cache: "public, max-age=31536000, immutable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			assert.Equal(t, tt.status, recorder.Code)
			assert.Equal(t, tt.contentType, recorder.Header().Get("Content-Type"))
			assert.Equal(t, tt.cache, recorder.Header().Get("Cache-Control"))
			assert.Equal(t, ContentSecurityPolicy, recorder.Header().Get("Content-Security-Policy"))
			assert.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
			assert.Equal(t, "no-referrer", recorder.Header().Get("Referrer-Policy"))
		})
	}
}

func TestHandlerCompressesStaticAssetsForBrowsers(t *testing.T) {
	distribution := completeDistribution()
	distribution["assets/index-a1b2c3.js"].Data = bytes.Repeat([]byte(`console.log("ready")`), 100)
	handler, err := NewHandler(distribution)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/assets/index-a1b2c3.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "gzip", recorder.Header().Get("Content-Encoding"))
	assert.Contains(t, recorder.Header().Values("Vary"), "Accept-Encoding")
	reader, err := gzip.NewReader(recorder.Body)
	require.NoError(t, err)
	decompressed, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, distribution["assets/index-a1b2c3.js"].Data, decompressed)
}

func TestHandlerDoesNotCompressWhenBrowserRejectsGzip(t *testing.T) {
	distribution := completeDistribution()
	distribution["assets/index-a1b2c3.js"].Data = bytes.Repeat([]byte(`console.log("ready")`), 100)
	handler, err := NewHandler(distribution)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/assets/index-a1b2c3.js", nil)
	req.Header.Set("Accept-Encoding", "gzip; q=0.0")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, recorder.Header().Get("Content-Encoding"))
	assert.Contains(t, recorder.Header().Values("Vary"), "Accept-Encoding")
	assert.Equal(t, distribution["assets/index-a1b2c3.js"].Data, recorder.Body.Bytes())
}

func TestHandlerRejectsNonNavigationFallbacks(t *testing.T) {
	handler, err := NewHandler(completeDistribution())
	require.NoError(t, err)

	paths := []string{
		"/missing.js",
		"/reviews/missing.js",
		"/api/status",
		"/openapi.json",
		"/debug/pprof/",
		"/reviews/../secret",
		`/reviews\\secret`,
		"/reviews/%00",
		"/.hidden",
	}
	for _, requestPath := range paths {
		t.Run(requestPath, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, requestPath, nil)
			req.Header.Set("Accept", "text/html")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			assert.Equal(t, http.StatusNotFound, recorder.Code)
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/analytics", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
}
