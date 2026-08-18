package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestUICmdOpensReviewRoutes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "reviews", want: "https://reviews.example.com/reviews"},
		{name: "job", args: []string{"42"}, want: "https://reviews.example.com/reviews/42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opened string
			withUICommandDependencies(t,
				func() error { return nil },
				func() (*daemon.RuntimeInfo, error) {
					return &daemon.RuntimeInfo{WebOrigin: "https://reviews.example.com"}, nil
				},
				func(target string) error { opened = target; return nil },
			)
			cmd := uiCmd()
			cmd.SetArgs(tt.args)

			require.NoError(t, cmd.Execute())
			assert.Equal(t, tt.want, opened)
			assert.NotContains(t, opened, "configured-secret")
		})
	}
}

func TestUICmdOpensPrefixedReviewRoutes(t *testing.T) {
	var opened string
	withUICommandDependencies(t,
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{
				WebOrigin:   "https://reviews.example.com",
				WebBasePath: "/roborev-ci",
			}, nil
		},
		func(target string) error { opened = target; return nil },
	)

	cmd := uiCmd()
	cmd.SetArgs([]string{"42"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "https://reviews.example.com/roborev-ci/reviews/42", opened)
}

func TestUICmdRejectsInvalidJobIDs(t *testing.T) {
	for _, args := range [][]string{{"0"}, {"-1"}, {"nope"}, {"1", "2"}} {
		cmd := uiCmd()
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}

func TestUICmdRequiresBrowserMetadata(t *testing.T) {
	withUICommandDependencies(t,
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) { return &daemon.RuntimeInfo{}, nil },
		func(string) error { return nil },
	)
	cmd := uiCmd()
	err := cmd.Execute()
	require.EqualError(t, err, "the daemon browser listener is disabled")
}

func TestUICmdPreservesDiscoveryAccessDenial(t *testing.T) {
	want := daemon.ErrDaemonAccessDenied
	withUICommandDependencies(t,
		func() error { return want },
		func() (*daemon.RuntimeInfo, error) { return nil, errors.New("unexpected discovery") },
		func(string) error { return nil },
	)
	cmd := uiCmd()
	err := cmd.Execute()
	require.ErrorIs(t, err, want)
}

func TestUICmdOpenerErrorIncludesManualURL(t *testing.T) {
	withUICommandDependencies(t,
		func() error { return nil },
		func() (*daemon.RuntimeInfo, error) {
			return &daemon.RuntimeInfo{WebOrigin: "https://reviews.example.com"}, nil
		},
		func(string) error { return errors.New("no opener") },
	)
	cmd := uiCmd()
	cmd.SetArgs([]string{"7"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://reviews.example.com/reviews/7")
}

func TestUICmdMatchesExplicitServerToItsRuntime(t *testing.T) {
	originalServerAddr := serverAddr
	originalParsed := parsedServerEndpoint
	originalList := uiListAllRuntimes
	originalDiscover := uiGetAnyRunningDaemon
	originalProbe := uiProbeDaemon
	serverAddr = "127.0.0.1:7474"
	parsedServerEndpoint = nil
	uiListAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{
			{Address: "127.0.0.1:7373", WebOrigin: "https://other.example.com"},
			{Address: "127.0.0.1:7474", WebOrigin: "https://selected.example.com"},
		}, nil
	}
	uiGetAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, errors.New("must not use arbitrary discovery with --server")
	}
	uiProbeDaemon = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
		return &daemon.PingInfo{OK: true, Service: "roborev", Version: "test"}, nil
	}
	t.Cleanup(func() {
		serverAddr = originalServerAddr
		parsedServerEndpoint = originalParsed
		uiListAllRuntimes = originalList
		uiGetAnyRunningDaemon = originalDiscover
		uiProbeDaemon = originalProbe
	})

	var opened string
	originalEnsure := uiEnsureDaemon
	originalOpen := openBrowserURL
	uiEnsureDaemon = func() error { return nil }
	openBrowserURL = func(target string) error { opened = target; return nil }
	t.Cleanup(func() {
		uiEnsureDaemon = originalEnsure
		openBrowserURL = originalOpen
	})

	require.NoError(t, uiCmd().Execute())
	assert.Equal(t, "https://selected.example.com/reviews", opened)
}

func TestUICmdMatchesEquivalentLoopbackServerToProbedDaemonPID(t *testing.T) {
	const livePID = 222
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(daemon.PingInfo{
			OK: true, Service: "roborev", Version: "test", PID: livePID,
		}))
	}))
	t.Cleanup(probe.Close)

	originalServerAddr := serverAddr
	originalParsed := parsedServerEndpoint
	originalList := uiListAllRuntimes
	originalOpen := openBrowserURL
	runtimeAddress := strings.TrimPrefix(probe.URL, "http://")
	serverAddr = strings.Replace(runtimeAddress, "127.0.0.1", "localhost", 1)
	parsedServerEndpoint = nil
	uiListAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{
			{PID: 111, Address: serverAddr, WebOrigin: "https://stale.example.com"},
			{PID: livePID, Address: runtimeAddress, WebOrigin: "https://live.example.com"},
		}, nil
	}
	var opened string
	openBrowserURL = func(target string) error { opened = target; return nil }
	t.Cleanup(func() {
		serverAddr = originalServerAddr
		parsedServerEndpoint = originalParsed
		uiListAllRuntimes = originalList
		openBrowserURL = originalOpen
	})

	require.NoError(t, uiCmd().Execute())
	assert.Equal(t, "https://live.example.com/reviews", opened)
}

func TestUICmdRejectsInvalidPublishedOrigin(t *testing.T) {
	for _, origin := range []string{
		"file:///tmp/reviews",
		"https://user@example.com",
		"https://reviews.example.com/path",
		"https://reviews.example.com?token=secret",
	} {
		withUICommandDependencies(t,
			func() error { return nil },
			func() (*daemon.RuntimeInfo, error) {
				return &daemon.RuntimeInfo{WebOrigin: origin}, nil
			},
			func(string) error { return nil },
		)
		cmd := uiCmd()
		require.Error(t, cmd.Execute(), origin)
	}
}

func withUICommandDependencies(
	t *testing.T,
	ensure func() error,
	discover func() (*daemon.RuntimeInfo, error),
	open func(string) error,
) {
	t.Helper()
	originalEnsure := uiEnsureDaemon
	originalDiscover := uiGetAnyRunningDaemon
	originalOpen := openBrowserURL
	originalServerAddr := serverAddr
	originalParsed := parsedServerEndpoint
	serverAddr = ""
	parsedServerEndpoint = nil
	uiEnsureDaemon = ensure
	uiGetAnyRunningDaemon = discover
	openBrowserURL = open
	t.Cleanup(func() {
		uiEnsureDaemon = originalEnsure
		uiGetAnyRunningDaemon = originalDiscover
		openBrowserURL = originalOpen
		serverAddr = originalServerAddr
		parsedServerEndpoint = originalParsed
	})
}
