package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/telemetry"
)

type observedCall struct {
	ep     DaemonEndpoint
	method string
	path   string
	status int
}

// recordObserver installs an observer that records each call and removes it when the test ends.
func recordObserver(t *testing.T) func() []observedCall {
	t.Helper()
	var mu sync.Mutex
	var calls []observedCall
	SetClientResponseObserver(func(ep DaemonEndpoint, req *http.Request, resp *http.Response) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, observedCall{ep: ep, method: req.Method, path: req.URL.Path, status: resp.StatusCode})
	})
	t.Cleanup(func() { SetClientResponseObserver(nil) })
	return func() []observedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]observedCall(nil), calls...)
	}
}

func TestClientResponseObserver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "full body")
	}))
	t.Cleanup(srv.Close)
	ep, err := ParseEndpoint(srv.URL)
	require.NoError(t, err)

	t.Run("inert without an observer", func(t *testing.T) {
		assert := assert.New(t)
		SetClientResponseObserver(nil)
		client := ep.HTTPClient(2 * time.Second)
		_, wrapped := client.Transport.(observedTransport)
		assert.False(wrapped)

		resp, err := client.Get(ep.BaseURL() + "/api/status")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal("full body", string(body))
	})

	t.Run("observes each completed round trip and passes the body through", func(t *testing.T) {
		assert := assert.New(t)
		calls := recordObserver(t)
		client := ep.HTTPClient(2 * time.Second)

		for _, path := range []string{"/api/status", "/api/jobs"} {
			resp, err := client.Get(ep.BaseURL() + path)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal("full body", string(body))
		}
		assert.Equal([]observedCall{
			{ep: ep, method: http.MethodGet, path: "/api/status", status: http.StatusOK},
			{ep: ep, method: http.MethodGet, path: "/api/jobs", status: http.StatusOK},
		}, calls())
	})

	t.Run("not called on a transport error", func(t *testing.T) {
		calls := recordObserver(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		closed := DaemonEndpoint{Network: "tcp", Address: ln.Addr().String()}
		require.NoError(t, ln.Close())

		_, err = closed.HTTPClient(2 * time.Second).Get(closed.BaseURL() + "/api/status")
		require.Error(t, err)
		assert.Empty(t, calls())
	})
}

func TestClientResponseObserverUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	assert := assert.New(t)
	sockPath := filepath.Join("/tmp", fmt.Sprintf("roborev-observer-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(sockPath) })
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	var mu sync.Mutex
	var seen []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/test" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	var observedEP DaemonEndpoint
	SetClientResponseObserver(func(ep DaemonEndpoint, req *http.Request, _ *http.Response) {
		// The telemetry client keeps this observer installed, so its own response re-enters here and is skipped.
		if req.URL.Path != "/test" {
			return
		}
		observedEP = ep
		telemetry.PostAppOpened(context.Background(), ep.HTTPClient(0), ep.BaseURL()+TelemetryEventsPath, telemetry.SurfaceCLI)
	})
	t.Cleanup(func() { SetClientResponseObserver(nil) })

	ep := DaemonEndpoint{Network: "unix", Address: sockPath}
	resp, err := ep.HTTPClient(2 * time.Second).Get(ep.BaseURL() + "/test")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal("ok", string(body))
	assert.Equal(DaemonEndpoint{Network: "unix", Address: sockPath}, observedEP)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal([]string{"GET /test", "POST " + TelemetryEventsPath}, seen)
}
