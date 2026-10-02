package daemon

import (
	"context"
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
	s := newAuthTestServer(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	httpServer := httptest.NewServer(s.httpServer.Handler)
	defer httpServer.Close()
	ep := authEndpoint(t, httpServer.URL)
	writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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
	writeAuthClientConfig(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	resp, err = client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_, err = ProbeDaemon(ep, time.Second)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	_, err = GetAnyRunningDaemonContext(context.Background())
	assert.ErrorIs(t, err, ErrDaemonAccessDenied)
}

func TestAuthClientRefusesOtherOriginsAndRedirects(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
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
	configText := `auth_key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
[web]
enabled = true
listen = "0.0.0.0:7373"
`
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(configText), 0o600))

	key, err := loadClientAuthKey()
	require.NoError(t, err)
	assert.Equal(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", key)
}

func TestAuthClientIgnoresUnrelatedConfigErrors(t *testing.T) {
	for _, setting := range []string{
		"[web]\nenabled = true\nlisten = \"not-an-address\"",
		"max_workers = \"not-a-number\"",
	} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("auth_key = \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n"+setting), 0o600))
			_, err := config.LoadGlobal()
			require.Error(t, err, "fixture must fail full config validation")
			s := newAuthTestServer(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
			server := httptest.NewServer(s.httpServer.Handler)
			defer server.Close()
			resp, err := authEndpoint(t, server.URL).HTTPClient(time.Second).Get(server.URL + "/api/ping")
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestAuthClientRejectsInvalidKeyConfigBeforeRequest(t *testing.T) {
	for _, contents := range []string{
		`auth_key = "a"`,
		`auth_key = "bad key"`,
		`auth_key = ["0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"]`,
		"auth_key = \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\ninvalid = [",
	} {
		t.Run(contents, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(contents), 0o600))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			resp, err := authEndpoint(t, server.URL).HTTPClient(time.Second).Get(server.URL + "/api/ping")
			require.ErrorIs(t, err, ErrClientConfig)
			assert.Nil(t, resp)
			assert.Zero(t, requests.Load())
			assert.NotContains(t, err.Error(), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
		})
	}
}

func TestAuthReadinessUsesCapturedCustomConfigKey(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	s := newAuthTestServer(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	server := httptest.NewServer(s.httpServer.Handler)
	defer server.Close()
	ep := authEndpoint(t, server.URL)
	ready, exited, err := waitForServerReady(context.Background(), ep, time.Second, make(chan error), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	assert.True(t, ready)
	assert.False(t, exited)
}

func TestAuthDeniedRuntimeIsPreserved(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	s := newAuthTestServer(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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
			writeAuthClientConfig(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "Bearer 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", r.Header.Get("Authorization"))
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
				assert.EqualValues(t, 1, requests.Load())
				return
			}
			require.ErrorIs(t, err, os.ErrNotExist)
			CleanupZombieDaemons(ep)
			assert.Zero(t, requests.Load(), "stale discovery and cleanup must not send credentials")
		})
	}
}
