package auth

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPClientScopesCredentialsAndPreservesRequest(t *testing.T) {
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/status", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(err)
		assert.Equal("synthetic review data", string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	original := server.Client()
	client := HTTPClient(server.URL, original, func() (string, error) { return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", nil })
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/status", strings.NewReader("synthetic review data"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer caller-key")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(http.StatusNoContent, resp.StatusCode)
	assert.Equal("Bearer caller-key", req.Header.Get("Authorization"))
	assert.Nil(original.CheckRedirect)
}

func TestHTTPClientRefusesRedirectsAndOtherOrigins(t *testing.T) {
	received := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received++; w.WriteHeader(204) }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := HTTPClient(origin.URL, nil, func() (string, error) { return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", nil })
	resp, err := client.Post(origin.URL, "application/json", strings.NewReader(`{"data":"private"}`))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	_, err = client.Get(other.URL)
	require.Error(t, err)
	assert.Zero(t, received)
}

func TestHTTPClientFailsBeforeSendingOnKeyError(t *testing.T) {
	received := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received++; w.WriteHeader(204) }))
	defer server.Close()
	expected := errors.New("credential config unavailable")
	client := HTTPClient(server.URL, nil, func() (string, error) { return "", expected })
	_, err := client.Get(server.URL)
	require.ErrorIs(t, err, expected)
	assert.Zero(t, received)
}

func TestEqualKey(t *testing.T) {
	assert := assert.New(t)
	assert.True(EqualKey("synthetic-bearer-key", "synthetic-bearer-key"))
	assert.False(EqualKey("synthetic-bearer-key", "synthetic-bearer-kez"))
	assert.False(EqualKey("synthetic-bearer-key", "short"))
}
