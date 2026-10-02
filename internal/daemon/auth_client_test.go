package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/internal/config"
)

func authEndpoint(t *testing.T, rawURL string) DaemonEndpoint {
	t.Helper()
	ep, err := ParseEndpoint(strings.TrimPrefix(rawURL, "http://"))
	require.NoError(t, err)
	return ep
}

func writeAuthClientConfig(t *testing.T, key string) {
	t.Helper()
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))
}

func TestAuthEndpointClientAndProbe(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	s := newAuthTestServer(t, "test-shared-key")
	httpServer := httptest.NewServer(s.httpServer.Handler)
	defer httpServer.Close()
	ep := authEndpoint(t, httpServer.URL)
	writeAuthClientConfig(t, "test-shared-key")
	require.NoError(t, WriteRuntime(ep, nil, "test-version", nil))
	client := ep.HTTPClient(time.Second)
	req, err := http.NewRequest(http.MethodGet, ep.BaseURL()+"/api/ping", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, req.Header.Get("Authorization"), "caller request must not be mutated")
	ping, err := ProbeDaemon(ep, time.Second)
	require.NoError(t, err)
	assert.True(t, ping.OK)
	writeAuthClientConfig(t, "wrong-key")
	resp, err = client.Do(req)
	assert.Nil(t, resp)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	_, err = ProbeDaemon(ep, time.Second)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	_, err = GetAnyRunningDaemonContext(context.Background())
	assert.ErrorIs(t, err, ErrDaemonAccessDenied)
}

func TestAuthClientRefusesOtherOriginsAndRedirects(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "test-shared-key")
	received := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := authEndpoint(t, origin.URL).HTTPClient(time.Second)
	resp, err := client.Get(target.URL)
	assert.Nil(t, resp)
	require.Error(t, err)
	resp, err = client.Post(origin.URL, "application/json", strings.NewReader(`{"secret":"review"}`))
	assert.Nil(t, resp)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	assert.Zero(t, received)
}

func TestAuthClientConfigFailureIsTerminal(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("auth_key = secret-never-print"), 0o600))
	_, err := GetAnyRunningDaemonContext(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrClientConfig)
	assert.True(t, IsDaemonAccessError(err))
	assert.NotContains(t, err.Error(), "access denied")
	assert.Contains(t, err.Error(), "config")
	assert.NotContains(t, err.Error(), "secret-never-print")
}

func TestAuthClientLoadsKeyDespiteUnrelatedSemanticConfigError(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	configText := `auth_key = "test-shared-key"
[web]
enabled = true
listen = "0.0.0.0:7373"
`
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(configText), 0o600))

	key, err := loadClientAuthKey()
	require.NoError(t, err)
	assert.Equal(t, "test-shared-key", key)
}

func TestAuthReadinessUsesCapturedCustomConfigKey(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "different-client-key")
	s := newAuthTestServer(t, "test-shared-key")
	server := httptest.NewServer(s.httpServer.Handler)
	defer server.Close()
	ep := authEndpoint(t, server.URL)
	ready, exited, err := waitForServerReady(context.Background(), ep, time.Second, make(chan error), "test-shared-key")
	require.NoError(t, err)
	assert.True(t, ready)
	assert.False(t, exited)
}

func TestAuthDeniedRuntimeIsPreserved(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	s := newAuthTestServer(t, "test-shared-key")
	server := httptest.NewServer(s.httpServer.Handler)
	defer server.Close()
	ep := authEndpoint(t, server.URL)
	require.NoError(t, WriteRuntime(ep, nil, "test-version", nil))
	_, err := GetAnyRunningDaemonContext(context.Background())
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	CleanupZombieDaemons(ep)
	_, err = os.Stat(RuntimePathForPID(os.Getpid()))
	assert.NoError(t, err)
}

func TestAuthDiscoverySkipsStaleProcesses(t *testing.T) {
	for _, state := range []string{"dead", "reused", "live"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			writeAuthClientConfig(t, "test-shared-key")
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/api/ping" && r.Header.Get("Authorization") == "" {
					assert.Empty(t, r.Header.Get("Authorization"))
					nonce, err := hex.DecodeString(r.Header.Get("X-Roborev-Auth-Nonce"))
					assert.NoError(t, err)
					w.Header().Set("X-Roborev-Auth-Proof", hex.EncodeToString(auth.ServerProof("test-shared-key", nonce)))
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				assert.Equal(t, "Bearer test-shared-key", r.Header.Get("Authorization"))
				fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, os.Getpid())
			}))
			defer server.Close()
			ep := authEndpoint(t, server.URL)
			rec := kitdaemon.NewRuntimeRecord(daemonServiceName, "test-version", ep.kitEndpoint())
			switch state {
			case "dead":
				rec.PID = math.MaxInt32
				require.False(t, kitdaemon.ProcessAlive(rec.PID))
			case "reused":
				identity, ok := kitdaemon.ReadProcessIdentity(os.Getppid())
				if !ok {
					t.Skip("parent process identity unavailable")
				}
				rec.ProcessIdentityV2 = identity
				require.Equal(t, kitdaemon.ProcessIdentityMismatch, kitdaemon.CompareRuntimeProcessIdentity(rec))
			}
			_, err := runtimeStore().Write(rec)
			require.NoError(t, err)
			_, err = GetAnyRunningDaemonContext(t.Context())
			if state == "live" {
				require.NoError(t, err)
				assert.EqualValues(t, 2, requests.Load())
				return
			}
			require.ErrorIs(t, err, os.ErrNotExist)
			CleanupZombieDaemons(ep)
			assert.Zero(t, requests.Load(), "stale discovery and cleanup must not send credentials")
		})
	}
}
