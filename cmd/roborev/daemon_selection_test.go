package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/version"
)

func isolateDaemonSelection(t *testing.T) {
	t.Helper()
	testenv.SetDataDir(t)
	oldAddr, oldParsed := serverAddr, parsedServerEndpoint
	oldDiscover, oldProbe := getAnyRunningDaemon, probeDaemonForEnsure
	oldCleanup, oldStart := cleanupZombieDaemons, startDaemonForEnsure
	oldService := findDaemonService
	serverAddr, parsedServerEndpoint = "", nil
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	t.Cleanup(func() {
		serverAddr, parsedServerEndpoint = oldAddr, oldParsed
		getAnyRunningDaemon, probeDaemonForEnsure = oldDiscover, oldProbe
		cleanupZombieDaemons, startDaemonForEnsure = oldCleanup, oldStart
		findDaemonService = oldService
	})
}

func TestEnsureDaemonDoesNotAdoptUnpublishedDefaultDaemon(t *testing.T) {
	for _, skip := range []string{"", "1"} {
		t.Run("skip-version="+skip, func(t *testing.T) {
			isolateDaemonSelection(t)
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", skip)
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
			var probes, starts int
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
				probes++
				return &daemon.PingInfo{OK: true, Service: "roborev", Version: version.Version}, nil
			}
			cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { return 0 }
			startDaemonForEnsure = func() error { starts++; return nil }
			require.NoError(t, ensureDaemon())
			assert.Zero(t, probes)
			assert.Equal(t, 1, starts)
		})
	}
}

func TestEnsureDaemonPropagatesDiscoveryIOError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"runtime directory failure", errors.New("runtime directory read failed")},
		{"access denied", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}},
		{"certificate failure", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDaemonSelection(t)
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, tc.err }
			var starts, probes, cleanups, services int
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
				probes++
				return nil, os.ErrNotExist
			}
			startDaemonForEnsure = func() error { starts++; return nil }
			cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanups++; return 0 }
			findDaemonService = func(context.Context, int) (*daemonService, error) { services++; return nil, nil }
			require.ErrorIs(t, ensureDaemon(), tc.err)
			assert.Zero(t, starts)
			assert.Zero(t, probes)
			assert.Zero(t, cleanups)
			assert.Zero(t, services)
		})
	}
}

func TestEnsureDaemonPropagatesDiscoveryTLSConfigError(t *testing.T) {
	isolateDaemonSelection(t)
	getAnyRunningDaemon = daemon.GetAnyRunningDaemon
	missing := filepath.Join(t.TempDir(), "missing.pem")
	configBody := fmt.Sprintf(
		"[daemon_tls]\n"+
			"ca_file = %q\n"+
			"client_cert_file = %q\n"+
			"client_key_file = %q\n",
		missing, missing, missing,
	)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(configBody), 0o600))
	endpoint := daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	require.NoError(t, daemon.WriteRuntime(endpoint, nil, version.Version, nil))

	var starts, cleanups int
	startDaemonForEnsure = func() error { starts++; return nil }
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanups++; return 0 }

	_, resolveErr := resolveDaemonEndpoint()
	require.ErrorIs(t, resolveErr, daemon.ErrClientConfig)
	require.ErrorIs(t, resolveErr, os.ErrNotExist)

	err := ensureDaemon()
	require.ErrorIs(t, err, daemon.ErrClientConfig)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Zero(t, starts)
	assert.Zero(t, cleanups)
}

func TestDaemonSelectionMissingRuntimeRejectsRequests(t *testing.T) {
	isolateDaemonSelection(t)
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(time.Second).Get(ep.BaseURL() + "/api/jobs")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, ErrDaemonNotRunning)
	require.ErrorContains(t, err, "no responsive daemon found")
}

func TestDaemonSelectionDiscoveryIOErrorRejectsRequests(t *testing.T) {
	isolateDaemonSelection(t)
	wantErr := errors.New("runtime directory read failed")
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, wantErr }
	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(time.Second).Get(ep.BaseURL() + "/api/jobs")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, wantErr)
}

func TestDaemonURLHelperDoesNotPromoteMissingRuntimeAddress(t *testing.T) {
	isolateDaemonSelection(t)
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	baseURL := getDaemonEndpoint().BaseURL()
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{Network: "tcp", Address: "127.0.0.1:2"}, nil
	}
	client := getDaemonHTTPClientForURL(baseURL, time.Second)
	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/jobs", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, errDaemonEndpointChanged)
}

func TestDaemonURLHelperPreservesDiscoveryErrorForPreviousAddress(t *testing.T) {
	isolateDaemonSelection(t)
	wantErr := errors.New("runtime directory read failed")
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, wantErr }
	baseURL := "http://127.0.0.1:2"
	resp, err := getDaemonHTTPClientForURL(baseURL, time.Second).Get(baseURL + "/api/jobs")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.ErrorIs(t, err, wantErr)
}

func TestDaemonURLHelperDoesNotDialUnselectedURL(t *testing.T) {
	discoveries := []struct {
		name     string
		discover func() (*daemon.RuntimeInfo, error)
		wantErr  error
	}{
		{
			name:     "no runtime",
			discover: func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist },
			wantErr:  ErrDaemonNotRunning,
		},
		{
			name: "daemon moved",
			discover: func() (*daemon.RuntimeInfo, error) {
				return &daemon.RuntimeInfo{Network: "tcp", Address: "127.0.0.1:2"}, nil
			},
			wantErr: errDaemonEndpointChanged,
		},
	}
	// Each URL may now belong to another local account: the shared default
	// port, its loopback aliases, or a port a stopped daemon published.
	urls := []string{"http://127.0.0.1:7373", "http://localhost:7373", "http://[::1]:7373", "http://127.0.0.1:7374"}
	for _, discovery := range discoveries {
		for _, baseURL := range urls {
			t.Run(discovery.name+"/"+baseURL, func(t *testing.T) {
				isolateDaemonSelection(t)
				getAnyRunningDaemon = discovery.discover
				oldTransport := http.DefaultTransport
				transport := &http.Transport{}
				dials := 0
				transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
					dials++
					return nil, errors.New("test intercepted network access")
				}
				http.DefaultTransport = transport
				t.Cleanup(func() { http.DefaultTransport = oldTransport; transport.CloseIdleConnections() })
				resp, err := getDaemonHTTPClientForURL(baseURL, time.Second).Get(baseURL + "/api/jobs")
				if resp != nil {
					_ = resp.Body.Close()
				}
				require.ErrorIs(t, err, discovery.wantErr)
				assert.Zero(t, dials)
			})
		}
	}
}

func TestDaemonURLHelperAcceptsOtherEndpointOfSelectedDaemon(t *testing.T) {
	isolateDaemonSelection(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	// The daemon publishes TCP and a Unix socket. A command saved the TCP URL
	// while the socket probe failed; discovery now answers on the socket.
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{
			Network:          "unix",
			Address:          filepath.Join(t.TempDir(), "daemon.sock"),
			AlternateNetwork: "tcp",
			AlternateAddress: strings.TrimPrefix(server.URL, "http://"),
		}, nil
	}
	resp, err := getDaemonHTTPClientForURL(server.URL, time.Second).Get(server.URL + "/api/jobs")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, requests)
}
