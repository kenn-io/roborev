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

func TestHTTPClientScopesCredentialsAndPreservesRequestOverVerifiedTLS(t *testing.T) {
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	original := server.Client()
	client := HTTPClient(server.URL, original, func() (string, error) { return "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", nil })
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/status", strings.NewReader("synthetic review data"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer caller-key")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", authorization)
	assert.Equal(t, "Bearer caller-key", req.Header.Get("Authorization"))
	assert.Nil(t, original.CheckRedirect)
}

func TestHTTPClientRefusesToSendKeyOrBodyOverPlainHTTP(t *testing.T) {
	var requests int
	var authorization string
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		authorization = r.Header.Get("Authorization")
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := HTTPClient(server.URL, server.Client(), func() (string, error) { return "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", nil })
	resp, err := client.Post(server.URL+"/api/status", "application/json", strings.NewReader("synthetic review data"))
	if resp != nil {
		defer resp.Body.Close()
	}
	require.ErrorIs(t, err, ErrPlaintextTransport)
	assert.Zero(t, requests)
	assert.Empty(t, authorization)
	assert.Empty(t, body)
}

func TestHTTPClientWithoutKeyPreservesPlainHTTP(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := HTTPClient(server.URL, server.Client(), func() (string, error) { return "", nil })
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, authorization)
}

func TestHTTPClientRefusesRedirectsAndOtherOrigins(t *testing.T) {
	otherRequests := 0
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRequests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := HTTPClient(origin.URL, origin.Client(), func() (string, error) { return "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", nil })
	resp, err := client.Post(origin.URL, "application/json", strings.NewReader(`{"data":"private"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	_, err = client.Get(other.URL)
	require.ErrorIs(t, err, ErrUnexpectedOrigin)
	assert.Zero(t, otherRequests)
}

func TestHTTPClientFailsBeforeSendingOnKeyError(t *testing.T) {
	received := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusNoContent)
	}))
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

func TestValidateKeyRequiresCanonical256BitHex(t *testing.T) {
	assert := assert.New(t)
	assert.NoError(ValidateKey(""), "an empty key keeps authentication disabled")
	key := "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	assert.NoError(ValidateKey(key))
	require.Error(t, ValidateKey("a"))
	require.Error(t, ValidateKey(strings.Repeat("a", 63)))
	require.Error(t, ValidateKey(strings.ToUpper(key)))
	require.Error(t, ValidateKey(key+"0"))
}
