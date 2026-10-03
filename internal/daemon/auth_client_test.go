package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math"
	"net"
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
	"go.kenn.io/roborev/internal/testutil"
)

func authEndpoint(t *testing.T, rawURL string) DaemonEndpoint {
	t.Helper()
	ep, err := ParseEndpoint(strings.TrimPrefix(rawURL, "http://"))
	require.NoError(t, err)
	return ep
}

func authTLSEndpoint(t *testing.T, server *httptest.Server) DaemonEndpoint {
	t.Helper()
	ep, err := ParseEndpoint(strings.TrimPrefix(server.URL, "https://"))
	require.NoError(t, err)
	ep.TLSCertPEM = string(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}))
	return ep
}

func writeAuthClientConfig(t *testing.T, key string) {
	t.Helper()
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "`+key+`"`), 0o600))
}

func TestAuthTCPUsesPinnedTLSFromRuntime(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	writeAuthClientConfig(t, key)
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.AuthKey = key
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	errCh, info := startServerAndWaitForRuntime(t, server)
	t.Cleanup(func() { stopTestServer(t, server, errCh) })

	assert.True(t, strings.HasPrefix(info.Endpoint().BaseURL(), "https://"))
	discovered, err := GetAnyRunningDaemonContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, info.PID, discovered.PID)
	client := discovered.Endpoint().HTTPClient(time.Second)
	resp, err := client.Get(discovered.Endpoint().BaseURL() + "/api/ping")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	plainResponse, err := http.Get("http://" + info.Address + "/api/ping")
	require.NoError(t, err)
	defer plainResponse.Body.Close()
	assert.NotEqual(t, http.StatusOK, plainResponse.StatusCode)
}

func TestAuthTCPUsesPinnedTLSForNonDefaultLoopbackPort(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	writeAuthClientConfig(t, key)
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	cfg.AuthKey = key
	cfg.Web.Enabled = false
	server := NewServer(db, cfg, "")
	serveErrCh, info := startServerAndWaitForRuntime(t, server)
	t.Cleanup(func() { stopTestServer(t, server, serveErrCh) })

	endpoint := info.Endpoint()
	assert.Equal(t, "https://"+info.Address, endpoint.BaseURL())
	resp, err := endpoint.HTTPClient(time.Second).Get(endpoint.BaseURL() + "/api/ping")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAuthHTTPClientAcceptsCurrentRuntimeCertificateAfterRestart(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	const key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
	writeAuthClientConfig(t, key)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	firstCert, firstCertPEM, err := newDaemonTLSCertificate(listener.Addr().String())
	require.NoError(t, err)
	var currentCert atomic.Pointer[tls.Certificate]
	currentCert.Store(&firstCert)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
	})}
	tlsListener := tls.NewListener(listener, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return currentCert.Load(), nil
		},
	})
	go func() { _ = server.Serve(tlsListener) }()
	t.Cleanup(func() { _ = server.Close() })

	ep := DaemonEndpoint{Network: "tcp", Address: listener.Addr().String(), TLSCertPEM: firstCertPEM}
	require.NoError(t, WriteRuntimeWithTLS(ep, nil, "test-version", nil, firstCertPEM))
	client := ep.HTTPClient(time.Second)
	getOK := func() {
		resp, err := client.Get(ep.BaseURL() + "/api/ping")
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}

	getOK()

	secondCert, secondCertPEM, err := newDaemonTLSCertificate(ep.Address)
	require.NoError(t, err)
	currentCert.Store(&secondCert)
	require.NoError(t, WriteRuntimeWithTLS(ep, nil, "test-version", nil, secondCertPEM))

	getOK()

	currentCert.Store(&firstCert)
	resp, err := client.Get(ep.BaseURL() + "/api/ping")
	assert.Nil(t, resp)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
}

func TestAuthClientRejectsTLSWithoutRuntimeCertificate(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	var received atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ep := authTLSEndpoint(t, server)
	resp, err := ep.HTTPClient(time.Second).Get(ep.BaseURL() + "/api/ping")
	assert.Nil(t, resp)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	assert.Zero(t, received.Load(), "the server must not receive a request without a runtime pin")
}

func TestVerifyCurrentDaemonCertificateAcceptsLocalhostAlias(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	certificate, certificatePEM, err := newDaemonTLSCertificate(listener.Addr().String())
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	require.NoError(t, err)
	runtimeEndpoint := DaemonEndpoint{
		Network:    "tcp",
		Address:    listener.Addr().String(),
		TLSCertPEM: certificatePEM,
	}
	require.NoError(t, WriteRuntimeWithTLS(runtimeEndpoint, nil, "test-version", nil, certificatePEM))

	_, port, err := net.SplitHostPort(runtimeEndpoint.Address)
	require.NoError(t, err)
	selectedEndpoint := runtimeEndpoint
	selectedEndpoint.Address = net.JoinHostPort("localhost", port)
	err = verifyCurrentDaemonCertificate(selectedEndpoint, tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf},
	})
	require.NoError(t, err)
}

func TestAuthEndpointClientAndProbe(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	s := newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	httpServer := httptest.NewTLSServer(s.httpServer.Handler)
	defer httpServer.Close()
	ep := authTLSEndpoint(t, httpServer)
	writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	require.NoError(t, WriteRuntimeWithTLS(ep, nil, "test-version", nil, ep.TLSCertPEM))
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
	writeAuthClientConfig(t, "5edae9b7c0eda3b75e0d34d9eda9607f54b4ac60f7e02e593dec0b2b1e98a5e0")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
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
	writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	received := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	ep := authTLSEndpoint(t, origin)
	require.NoError(t, WriteRuntimeWithTLS(ep, nil, "test-version", nil, ep.TLSCertPEM))
	client := ep.HTTPClient(time.Second)
	resp, err := client.Get(target.URL)
	assert.Nil(t, resp)
	require.Error(t, err)
	resp, err = client.Post(origin.URL, "application/json", strings.NewReader(`{"secret":"review"}`))
	require.NoError(t, err)
	require.NotNil(t, resp)
	resp.Body.Close()
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.Zero(t, received)
}

func TestAuthClientRejectsUnpinnedTLSCertificate(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	var received atomic.Int32
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer untrusted.Close()

	ep := authTLSEndpoint(t, untrusted)
	_, certificatePEM, err := newDaemonTLSCertificate("127.0.0.1:7373")
	require.NoError(t, err)
	ep.TLSCertPEM = certificatePEM
	resp, err := ep.HTTPClient(time.Second).Get(ep.BaseURL() + "/api/ping")
	assert.Nil(t, resp)
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	assert.Zero(t, received.Load(), "the untrusted server must not receive the authenticated request")
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
	configText := `auth_key = "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"
[web]
enabled = true
listen = "0.0.0.0:7373"
`
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(configText), 0o600))

	key, err := loadClientAuthKey()
	require.NoError(t, err)
	assert.Equal(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", key)
}

func TestAuthClientIgnoresUnrelatedConfigErrors(t *testing.T) {
	for _, setting := range []string{
		"[web]\nenabled = true\nlisten = \"not-an-address\"",
		"max_workers = \"not-a-number\"",
	} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("auth_key = \"51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015\"\n"+setting), 0o600))
			_, err := config.LoadGlobal()
			require.Error(t, err, "fixture must fail full config validation")
			s := newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
			server := httptest.NewTLSServer(s.httpServer.Handler)
			defer server.Close()
			endpoint := authTLSEndpoint(t, server)
			require.NoError(t, WriteRuntimeWithTLS(endpoint, nil, "test-version", nil, endpoint.TLSCertPEM))
			resp, err := endpoint.HTTPClient(time.Second).Get(endpoint.BaseURL() + "/api/ping")
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
		})
	}
}

func TestAuthClientRejectsInvalidKeyConfigBeforeRequest(t *testing.T) {
	for _, contents := range []string{
		`auth_key = "bad key"`,
		`auth_key = ["51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"]`,
		"auth_key = \"51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015\"\ninvalid = [",
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
			assert.NotContains(t, err.Error(), "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
		})
	}
}

func TestAuthReadinessUsesCapturedCustomConfigKey(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "5edae9b7c0eda3b75e0d34d9eda9607f54b4ac60f7e02e593dec0b2b1e98a5e0")
	s := newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	server := httptest.NewTLSServer(s.httpServer.Handler)
	defer server.Close()
	ep := authTLSEndpoint(t, server)
	ep.allowTLSBootstrap = true
	ready, exited, err := waitForServerReady(context.Background(), ep, time.Second, make(chan error), "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	require.NoError(t, err)
	assert.True(t, ready)
	assert.False(t, exited)
}

func TestAuthDeniedRuntimeIsPreserved(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	s := newAuthTestServer(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	server := httptest.NewTLSServer(s.httpServer.Handler)
	defer server.Close()
	ep := authTLSEndpoint(t, server)
	require.NoError(t, WriteRuntimeWithTLS(ep, nil, "test-version", nil, ep.TLSCertPEM))
	_, err := GetAnyRunningDaemonContext(context.Background())
	require.ErrorIs(t, err, ErrDaemonAccessDenied)
	CleanupZombieDaemons(ep)
	_, err = os.Stat(RuntimePathForPID(os.Getpid()))
	assert.NoError(t, err)
}

func TestAuthPlaintextRuntimeIsPreserved(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
	ep := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	require.NoError(t, WriteRuntime(ep, nil, "test-version", nil))

	_, discoveryErr := GetAnyRunningDaemonContext(t.Context())
	require.ErrorIs(t, discoveryErr, ErrPlaintextAuthTransport)
	assert.True(t, IsDaemonAccessError(discoveryErr))

	CleanupZombieDaemons(ep)
	assert.FileExists(t, RuntimePathForPID(os.Getpid()))
}

func TestAuthUnverifiedTLSRuntimeIsPreserved(t *testing.T) {
	_, mismatchedCert, err := newDaemonTLSCertificate("127.0.0.1:7373")
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		certificate string
		key         string
	}{
		{name: "mismatched certificate", certificate: mismatchedCert, key: "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"},
		{name: "mismatched certificate without key", certificate: mismatchedCert},
		{name: "malformed certificate", certificate: "not a certificate", key: "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			writeAuthClientConfig(t, tc.key)
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, os.Getpid())
			}))
			defer server.Close()
			ep := authTLSEndpoint(t, server)
			ep.TLSCertPEM = tc.certificate
			require.NoError(t, WriteRuntimeWithTLS(ep, nil, "test-version", nil, ep.TLSCertPEM))

			_, discoveryErr := GetAnyRunningDaemonContext(t.Context())
			require.ErrorIs(t, discoveryErr, ErrDaemonAccessDenied)
			assert.Zero(t, requests.Load())
			CleanupZombieDaemons(ep)
			assert.FileExists(t, RuntimePathForPID(os.Getpid()))
		})
	}
}

func TestAuthDiscoverySkipsStaleProcesses(t *testing.T) {
	for _, state := range []string{"dead", "reused", "live"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			writeAuthClientConfig(t, "51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015")
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "Bearer 51085fd49ac22900a0839b036090b4ea2050c5911e0edcbe0b8f7fed5a096015", r.Header.Get("Authorization"))
				fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, os.Getpid())
			}))
			defer server.Close()
			ep := authTLSEndpoint(t, server)
			rec := kitdaemon.NewRuntimeRecord(daemonServiceName, "test-version", ep.kitEndpoint())
			rec.Metadata = map[string]string{runtimeTLSCertificateKey: ep.TLSCertPEM}
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
