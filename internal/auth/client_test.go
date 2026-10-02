package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serverProofForTest(key string, nonce []byte) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("roborev-daemon-auth-v1:server:"))
	_, _ = mac.Write(nonce)
	return mac.Sum(nil)
}

func writeServerProofForTest(w http.ResponseWriter, key string, nonce []byte) {
	w.Header().Set("X-Roborev-Auth-Proof", hex.EncodeToString(serverProofForTest(key, nonce)))
	w.WriteHeader(http.StatusUnauthorized)
}

func TestHTTPClientScopesCredentialsAndPreservesRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			assert.Empty(t, r.Header.Get("Authorization"))
			nonce, err := hex.DecodeString(r.Header.Get("X-Roborev-Auth-Nonce"))
			assert.NoError(t, err)
			writeServerProofForTest(w, "test-shared-key", nonce)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-shared-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	original := server.Client()
	client := HTTPClient(server.URL, original, func() (string, error) { return "test-shared-key", nil })
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer caller-key")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "Bearer caller-key", req.Header.Get("Authorization"))
	assert.Nil(t, original.CheckRedirect)
}

func TestHTTPClientAuthenticatesServerBeforeSendingKey(t *testing.T) {
	const key = "test-shared-key"
	challengeCalls := 0
	authorizedCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			challengeCalls++
			assert.Empty(t, r.Header.Get("Authorization"))
			nonce, err := hex.DecodeString(r.Header.Get("X-Roborev-Auth-Nonce"))
			assert.NoError(t, err)
			assert.NotEmpty(t, nonce)
			writeServerProofForTest(w, key, nonce)
			return
		}
		if r.URL.Path == "/api/status" {
			authorizedCalls++
			assert.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := HTTPClient(server.URL, server.Client(), func() (string, error) { return key, nil })
	resp, err := client.Get(server.URL + "/api/status")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, 1, challengeCalls)
	assert.Equal(t, 1, authorizedCalls)
}

func TestHTTPClientSendsNoCredentialsToUnverifiedServer(t *testing.T) {
	var authorization string
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authorization = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := HTTPClient(server.URL, server.Client(), func() (string, error) { return "test-shared-key", nil })
	resp, err := client.Post(server.URL+"/api/status", "application/json", strings.NewReader("synthetic review data"))
	if resp != nil {
		resp.Body.Close()
	}
	require.ErrorIs(t, err, ErrUnverifiedServer)
	assert.Empty(t, authorization)
	assert.Empty(t, body)
}

func TestHTTPClientRejectsReplayedServerProof(t *testing.T) {
	const key = "test-shared-key"
	challengeCalls := 0
	authorizedCalls := 0
	var firstProof string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			challengeCalls++
			if challengeCalls == 1 {
				nonce, err := hex.DecodeString(r.Header.Get("X-Roborev-Auth-Nonce"))
				assert.NoError(t, err)
				firstProof = hex.EncodeToString(serverProofForTest(key, nonce))
			}
			w.Header().Set("X-Roborev-Auth-Proof", firstProof)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/status" {
			authorizedCalls++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := HTTPClient(server.URL, server.Client(), func() (string, error) { return key, nil })
	resp, err := client.Get(server.URL + "/api/status")
	require.NoError(t, err)
	resp.Body.Close()
	_, err = client.Get(server.URL + "/api/status")
	require.ErrorIs(t, err, ErrUnverifiedServer)
	assert.Equal(t, 2, challengeCalls)
	assert.Equal(t, 1, authorizedCalls)
}

func TestHTTPClientRefusesRedirectsAndOtherOrigins(t *testing.T) {
	received := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received++; w.WriteHeader(204) }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := HTTPClient(origin.URL, nil, func() (string, error) { return "test-key", nil })
	resp, err := client.Post(origin.URL, "application/json", strings.NewReader(`{"data":"private"}`))
	require.ErrorIs(t, err, ErrUnverifiedServer)
	assert.Nil(t, resp)
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
