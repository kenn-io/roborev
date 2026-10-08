package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

type reviewPipeListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (l *reviewPipeListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *reviewPipeListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		_ = l.conn.Close()
	})
	return nil
}

func (*reviewPipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7373}
}

func TestWaitForJobCompletionReturnsNotFoundImmediately(t *testing.T) {
	var jobCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			jobCalls++
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	review, err := waitForJobCompletion(ctx, server.URL, 123, nil)
	require.Error(t, err)
	assert.Nil(t, review)
	require.ErrorIs(t, err, ErrJobNotFound)
	assert.Equal(t, 1, jobCalls, "expected not-found to fail fast instead of polling until timeout")
}

func TestAuthJobPollingStopsOnTerminalDenial(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       storage.JobStatus
		brokenConfig bool
		calls        int
	}{
		{"job denied", "", false, 1},
		{"partial review denied", storage.JobStatusRunning, false, 2},
		{"finished review denied", storage.JobStatusDone, false, 2},
		{"invalid config", "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			if tc.brokenConfig {
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(`auth_key = "invalid key"`), 0o600))
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/api/jobs" && tc.status != "" {
					writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 23, Status: tc.status}}})
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			review, err := waitForJobCompletion(ctx, server.URL, 23, io.Discard)
			wantErr := daemon.ErrDaemonAccessDenied
			if tc.brokenConfig {
				wantErr = daemon.ErrClientConfig
			}
			require.ErrorIs(t, err, wantErr)
			assert.Nil(t, review)
			assert.Equal(t, tc.calls, calls)
		})
	}
}

func TestWaitForJobCompletionRecoversWhenRuntimeAppears(t *testing.T) {
	isolateDaemonSelection(t)
	oldTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	var jobCalls, reviewCalls int

	var discoveries int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		discoveries++
		if discoveries == 1 {
			return nil, os.ErrNotExist
		}
		return &daemon.RuntimeInfo{Network: "tcp", Address: "127.0.0.1:7373"}, nil
	}

	synctest.Test(t, func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/jobs":
				jobCalls++
				writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 123, Status: storage.JobStatusDone}}})
			case "/api/review":
				reviewCalls++
				writeJSON(w, storage.Review{JobID: 123, Output: "completed"})
			default:
				http.NotFound(w, r)
			}
		})
		serverConn, clientConn := net.Pipe()
		listener := &reviewPipeListener{conn: serverConn, closed: make(chan struct{})}
		server := &http.Server{Handler: handler}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		transport := &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
		}
		http.DefaultTransport = transport
		defer func() {
			serverErr := server.Close()
			serveErr := <-serveDone
			assert.NoError(t, clientConn.Close())
			require.NoError(t, serverErr)
			require.ErrorIs(t, serveErr, http.ErrServerClosed)
		}()

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		review, err := waitForJobCompletion(ctx, defaultDaemonEndpoint().BaseURL(), 123, nil)
		require.NoError(t, err)
		require.NotNil(t, review)
		assert.Equal(t, "completed", review.Output)
	})
	assert.Equal(t, 2, discoveries)
	assert.Equal(t, 1, jobCalls)
	assert.Equal(t, 1, reviewCalls)
}

func TestWaitForJobCompletionPropagatesDiscoveryError(t *testing.T) {
	isolateDaemonSelection(t)
	wantErr := errors.New("runtime directory read failed")
	var discoveries int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		discoveries++
		return nil, wantErr
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := waitForJobCompletion(ctx, defaultDaemonEndpoint().BaseURL(), 123, nil)
		require.ErrorIs(t, err, wantErr)
	})
	assert.Equal(t, 1, discoveries)
}

func TestWaitForJobCompletionPropagatesStaleDefaultURL(t *testing.T) {
	isolateDaemonSelection(t)
	var discoveries int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		discoveries++
		return &daemon.RuntimeInfo{Network: "tcp", Address: "127.0.0.1:7474"}, nil
	}

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := waitForJobCompletion(ctx, defaultDaemonEndpoint().BaseURL(), 123, nil)
		require.ErrorIs(t, err, ErrDaemonNotRunning)
	})
	assert.Equal(t, 1, discoveries)
}
