package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWithHTTPClientNormalizesBaseURL(t *testing.T) {
	assert := assert.New(t)

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL+"/", server.Client())
	require.NoError(t, err)

	resp, err := api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal("/api/ping", gotPath)
}
