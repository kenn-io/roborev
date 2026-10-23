package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/version"
)

func TestEnsureDaemonDoesNotSpawnWithRespondingUnjudgedRuntime(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprintf("denied=%t", denied), func(t *testing.T) {
			isolateDaemonSelection(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			cfg := config.DefaultConfig()
			cfg.AuthKey = strings.Repeat("ab", 32)
			require.NoError(t, config.SaveGlobal(cfg))
			rec := testutil.UnknownRuntimeRecord("tcp", "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.Header.Get("Authorization"))
				if denied {
					w.Header().Set("WWW-Authenticate", "Bearer")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = fmt.Fprint(w, `{"error":"daemon authentication required"}`)
					return
				}
				fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, rec.PID)
			}))
			t.Cleanup(server.Close)
			rec.Address = server.Listener.Addr().String()
			path, err := daemon.RuntimeStore().Write(rec)
			require.NoError(t, err)
			starts := 0
			startDaemonForEnsure = func() error { starts++; return nil }
			err = ensureDaemon()
			assert := assert.New(t)
			require.ErrorIs(t, err, daemon.ErrUnjudgedRuntime)
			assert.Contains(err.Error(), filepath.Base(path))
			assert.Contains(err.Error(), fmt.Sprintf("pid %d", rec.PID))
			assert.Contains(err.Error(), "running in another PID namespace; not managed from here")
			assert.Zero(starts)
		})
	}
}

func isolateDaemonSelection(t *testing.T) {
	t.Helper()
	testenv.SetDataDir(t)
	oldAddr, oldParsed := serverAddr, parsedServerEndpoint
	oldDiscover, oldProbe := getAnyRunningDaemon, probeDaemonForEnsure
	oldCleanup, oldStart := cleanupZombieDaemons, startDaemonForEnsure
	oldService, oldCgroup := findDaemonService, daemonCgroupUnit
	serverAddr, parsedServerEndpoint = "", nil
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	daemonCgroupUnit = func(int) (string, error) { return "", nil }
	t.Cleanup(func() {
		serverAddr, parsedServerEndpoint = oldAddr, oldParsed
		getAnyRunningDaemon, probeDaemonForEnsure = oldDiscover, oldProbe
		cleanupZombieDaemons, startDaemonForEnsure = oldCleanup, oldStart
		findDaemonService, daemonCgroupUnit = oldService, oldCgroup
	})
}

func TestEnsureDaemonAllowsUnjudgedEndpointReusedByUnauthorizedService(t *testing.T) {
	isolateDaemonSelection(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.AuthKey = strings.Repeat("ab", 32)
	require.NoError(t, config.SaveGlobal(cfg))
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(foreign.Close)
	rec := testutil.UnknownRuntimeRecord("tcp", foreign.Listener.Addr().String())
	path, err := daemon.RuntimeStore().Write(rec)
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	getAnyRunningDaemon = daemon.GetAnyRunningDaemon
	starts := 0
	startDaemonForEnsure = func() error { starts++; return nil }

	require.NoError(t, ensureDaemon())
	assert.Equal(t, 1, starts)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestEnsureDaemonDoesNotAdoptUnpublishedDefaultDaemon(t *testing.T) {
	for _, skip := range []string{"", "1"} {
		t.Run("skip-version="+skip, func(t *testing.T) {
			isolateDaemonSelection(t)
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", skip)
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
			var probes, starts int
			var events []string
			probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
				probes++
				return &daemon.PingInfo{OK: true, Service: "roborev", Version: version.Version}, nil
			}
			cleanupZombieDaemons = func(ep daemon.DaemonEndpoint) int { events = append(events, "cleanup:"+ep.Address); return 1 }
			startDaemonForEnsure = func() error { starts++; events = append(events, "start"); return nil }
			require.NoError(t, ensureDaemon())
			assert.Zero(t, probes)
			assert.Equal(t, 1, starts)
			assert.Equal(t, []string{"cleanup:127.0.0.1:1", "start"}, events)
		})
	}
}

func TestEnsureDaemonPropagatesDiscoveryIOError(t *testing.T) {
	discoveryErr := errors.New("runtime directory read failed")
	isolateDaemonSelection(t)
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, discoveryErr }
	var starts, probes, cleanups, services int
	probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		probes++
		return nil, os.ErrNotExist
	}
	startDaemonForEnsure = func() error { starts++; return nil }
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { cleanups++; return 0 }
	findDaemonService = func(context.Context, int) (*daemonService, error) { services++; return nil, nil }
	require.ErrorIs(t, ensureDaemon(), discoveryErr)
	assert.Zero(t, starts)
	assert.Zero(t, probes)
	assert.Zero(t, cleanups)
	assert.Zero(t, services)
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
