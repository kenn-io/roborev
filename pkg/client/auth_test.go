package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestAuthKeyTypedAndRawClient(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/ping" {
			_, _ = w.Write([]byte(`{"ok":true,"service":"roborev"}`))
			return
		}
		_, _ = w.Write([]byte("raw-log"))
	}))
	defer server.Close()
	api, err := NewWithAuthKeyAndHTTPClient(server.URL, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", server.Client())
	require.NoError(t, err)
	ping, err := api.Ping(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ping)
	// Any successful typed response proves the authorization was attached.
	resp, err := api.GetJobLogRaw(context.Background(), &generated.GetJobLogRequestOptions{})
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAuthKeyClientRefusesPlainHTTP(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	api, err := NewWithAuthKey(server.URL, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	require.NoError(t, err)
	_, err = api.Ping(context.Background())
	require.ErrorIs(t, err, auth.ErrPlaintextTransport)
	assert.Zero(t, requests)
}
