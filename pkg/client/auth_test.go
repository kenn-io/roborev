package client

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestAuthKeyTypedAndRawClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" && r.Header.Get("Authorization") == "" {
			nonce, err := hex.DecodeString(r.Header.Get("X-Roborev-Auth-Nonce"))
			assert.NoError(t, err)
			w.Header().Set("X-Roborev-Auth-Proof", hex.EncodeToString(auth.ServerProof("test-shared-key", nonce)))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-shared-key" {
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
	api, err := NewWithAuthKey(server.URL, "test-shared-key")
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
