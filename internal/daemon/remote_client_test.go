package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/requestsigning"
)

func TestRemoteEndpointUsesNativeSignedClient(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	key := requestsigning.Key{ID: "reader", Secret: secret}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "the daemon auth_key must stay on the server")
		_, err := requestsigning.VerifyHeaders(r, "https://"+r.Host+r.URL.RequestURI(), map[string]requestsigning.Key{key.ID: key}, time.Now())
		assert.NoError(t, err)
		assert.Equal(t, "/history/api/ping", r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true,"service":"roborev","version":"test"}`))
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	t.Setenv("TEST_REMOTE_SIGNING_KEY", hex.EncodeToString(secret))
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "auth_key = '%s'\n[remote_client]\nexternal_url='%s'\nkey_id='reader'\nsecret_env='TEST_REMOTE_SIGNING_KEY'\nca_file='%s'\n", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", server.URL+"/history", caFile), 0o600))
	ep, err := ParseEndpoint(server.URL + "/history")
	require.NoError(t, err)
	assert.True(t, ep.IsRemote())
	assert.Equal(t, server.URL+"/history", ep.BaseURL())
	resp, err := ep.APIClient(time.Second).PingRaw(t.Context())
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	// Selecting another HTTPS origin or prefix cannot reuse these credentials.
	_, err = ParseEndpoint("https://other.example.com/history")
	require.ErrorContains(t, err, "differs from configured credential origin")
	_, err = ParseEndpoint(server.URL + "/different-prefix")
	require.Error(t, err)
	pinned := ep.HTTPClient(30 * time.Second)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "[remote_client]\nexternal_url='https://other.example.com/history'\nkey_id='reader'\nsecret_env='TEST_REMOTE_SIGNING_KEY'\nca_file='%s'\n", caFile), 0o600))
	_, err = pinned.Get(ep.BaseURL() + "/api/ping")
	require.Error(t, err, "reload must not move a different origin's credential to the existing client")
	require.NoError(t, os.Remove(caFile))
	_, err = ep.HTTPClient(time.Second).Get(ep.BaseURL() + "/api/ping")
	require.Error(t, err)
	_, err = ParseEndpoint("http://reviews.example.com/history")
	require.Error(t, err)
}
