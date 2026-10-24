package kata

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func transportFor(t *testing.T, rawURL, key string, mutate ...func(*transportOptions)) *transport {
	t.Helper()
	endpoint, err := config.ParseKataEndpoint(rawURL)
	require.NoError(t, err)
	opts := transportOptions{Endpoint: endpoint, APIKey: key}
	for _, m := range mutate {
		m(&opts)
	}
	tr, err := newTransport(opts)
	require.NoError(t, err)
	return tr
}

func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

type pong struct {
	OK bool `json:"ok"`
}

func TestTransportSendsBearerAndAcceptHeaders(t *testing.T) {
	var got http.Header
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotPath, gotQuery = r.Header.Clone(), r.URL.Path, r.URL.RawQuery
		jsonHandler(`{"ok":true}`)(w, r)
	}))
	defer srv.Close()
	tr := transportFor(t, srv.URL+"/proxy/", "static-credential")

	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/api/v1/health", url.Values{"a": {"b c"}}, nil, nil, &out, http.StatusOK)
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.Equal(t, "Bearer static-credential", got.Get("Authorization"))
	assert.Equal(t, "application/json", got.Get("Accept"))
	assert.Equal(t, "/proxy/api/v1/health", gotPath, "a path prefix on the endpoint is kept")
	assert.Equal(t, "a=b+c", gotQuery)
}

func TestTransportSendsNoAuthorizationWithoutAKey(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		jsonHandler(`{"ok":true}`)(w, r)
	}))
	defer srv.Close()
	// An https endpoint may rely on mutual TLS or an authenticating proxy, so
	// it can run without a key; the server decides.
	tr := transportFor(t, "https://kata.example.com", "", func(o *transportOptions) {
		o.HTTPClient = srv.Client()
		o.Endpoint.URL, _ = url.Parse(srv.URL)
	})
	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
	require.NoError(t, err)
	assert.Empty(t, got.Get("Authorization"))
}

func TestTransportEncodesJSONBodiesDeterministically(t *testing.T) {
	var gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody, gotType = string(buf[:n]), r.Header.Get("Content-Type")
		jsonHandler(`{"ok":true}`)(w, r)
	}))
	defer srv.Close()
	tr := transportFor(t, srv.URL, "k")
	var out pong
	_, err := tr.do(t.Context(), http.MethodPost, "/x", nil, map[string]any{"b": 1, "a": 2}, http.Header{"Idempotency-Key": {"k1"}}, &out, http.StatusOK)
	require.NoError(t, err)
	assert.Equal(t, `{"a":2,"b":1}`, gotBody, "keys are ordered so a retry sends identical bytes")
	assert.Equal(t, "application/json", gotType)
}

func TestTransportRejectsRedirectsWithoutFollowingThem(t *testing.T) {
	var reached atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		assert.Empty(t, r.Header.Get("Authorization"), "the credential must not follow a redirect")
		jsonHandler(`{"ok":true}`)(w, r)
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	tr := transportFor(t, srv.URL, "static-credential")

	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/api/v1/health", nil, nil, nil, &out, http.StatusOK)
	require.ErrorIs(t, err, ErrRedirect)
	assert.False(t, reached.Load())
	assert.NotContains(t, err.Error(), "static-credential")
}

// roundTripFunc stands in for a server that accepts the request and never
// answers; real sockets are not durably blocking inside a synctest bubble.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportPreservesCallerDeadlineBeforeResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		started := make(chan struct{})
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		tr := transportFor(t, "https://kata.example.com", "k", func(o *transportOptions) {
			o.HTTPClient = client
		})
		errC := make(chan error, 1)
		go func() {
			var out pong
			_, err := tr.do(ctx, http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
			errC <- err
		}()
		<-started
		synctest.Wait()
		assert.Empty(t, errC)
		require.ErrorIs(t, <-errC, context.DeadlineExceeded)
	})
}

// cancellationBody signals only once the transport begins reading the response.
// A distinct read error ensures do must recover the caller's cancellation.
type cancellationBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *cancellationBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, io.ErrUnexpectedEOF
}

func (*cancellationBody) Close() error { return nil }

func TestTransportPreservesCallerCancellationWhileReadingBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &cancellationBody{ctx: r.Context(), started: started},
			}, nil
		})}
		tr := transportFor(t, "https://kata.example.com", "k", func(o *transportOptions) {
			o.HTTPClient = client
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		errC := make(chan error, 1)
		go func() {
			var out pong
			_, err := tr.do(ctx, http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
			errC <- err
		}()
		<-started
		cancel()
		require.ErrorIs(t, <-errC, context.Canceled)
	})
}

func TestTransportClassifiesStatuses(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusUnauthorized:        ErrAuthenticationRequired,
		http.StatusForbidden:           ErrAuthenticationRequired,
		http.StatusNotFound:            ErrNotFound,
		http.StatusConflict:            ErrConflict,
		http.StatusPreconditionFailed:  ErrConflict,
		http.StatusBadRequest:          ErrRequestRejected,
		http.StatusUnprocessableEntity: ErrRequestRejected,
		// Kata accepts a write of at most 1 MiB; a larger one is the caller's
		// payload to shrink, not a sign of an incompatible server.
		http.StatusRequestEntityTooLarge:       ErrRequestRejected,
		http.StatusRequestHeaderFieldsTooLarge: ErrRequestRejected,
		http.StatusTooManyRequests:             ErrUnreachable,
		http.StatusBadGateway:                  ErrUnreachable,
		http.StatusServiceUnavailable:          ErrUnreachable,
		http.StatusNotImplemented:              ErrIncompatible,
		http.StatusTeapot:                      ErrIncompatible,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		tr := transportFor(t, srv.URL, "k")
		var out pong
		_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
		srv.Close()
		require.ErrorIs(t, err, want, "status %d", status)
	}
}

func TestTransportReportsKataErrorCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"error":{"code":"idempotency_mismatch","message":"key reused with a different body"}}`))
	}))
	defer srv.Close()
	tr := transportFor(t, srv.URL, "k")
	var out pong
	_, err := tr.do(t.Context(), http.MethodPost, "/x", nil, nil, nil, &out, http.StatusOK)
	require.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, "idempotency_mismatch", ErrorCode(err))
	apiErr, ok := AsAPIError(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Contains(t, apiErr.Error(), "idempotency_mismatch")
	assert.Empty(t, ErrorCode(ErrRedirect))
}

func TestTransportPreservesLongServerMessages(t *testing.T) {
	long := strings.Repeat("m", (64<<10)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad","message":"` + long + `"}}`))
	}))
	defer srv.Close()
	tr := transportFor(t, srv.URL, "k")
	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
	require.ErrorIs(t, err, ErrRequestRejected)
	apiErr, ok := AsAPIError(err)
	require.True(t, ok)
	assert.Equal(t, long, apiErr.Message)
}

func TestTransportRejectsMalformedSuccessBodies(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>`,
		"trailing data": `{"ok":true} {"ok":false}`,
		"wrong type":    `{"ok":"yes"}`,
		"empty":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(jsonHandler(body))
			defer srv.Close()
			tr := transportFor(t, srv.URL, "k")
			var out pong
			_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestTransportToleratesFieldsFromNewerServers(t *testing.T) {
	srv := httptest.NewServer(jsonHandler(`{"ok":true,"a_field_added_in_a_later_kata":{"x":1}}`))
	defer srv.Close()
	tr := transportFor(t, srv.URL, "k")
	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
	require.NoError(t, err)
	assert.True(t, out.OK)
}

func TestTransportReportsAClosedPortAsUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())
	tr := transportFor(t, "http://"+addr, "k")
	var out pong
	_, err = tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
	require.ErrorIs(t, err, ErrUnreachable)
}

func TestNewTransportRequiresAKeyForLoopbackHTTP(t *testing.T) {
	endpoint, err := config.ParseKataEndpoint("http://127.0.0.1:7777")
	require.NoError(t, err)
	_, err = newTransport(transportOptions{Endpoint: endpoint})
	require.ErrorIs(t, err, ErrAuthenticationRequired)
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes, which t.TempDir() can
	// exceed on macOS.
	dir, err := os.MkdirTemp("/tmp", "rk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func serveOnUnixSocket(t *testing.T, dirMode, socketMode os.FileMode, handler http.Handler) string {
	t.Helper()
	dir := shortTempDir(t)
	require.NoError(t, os.Chmod(dir, dirMode))
	path := filepath.Join(dir, "k.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, socketMode))
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

func TestTransportReachesAPrivateUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket ownership checks are unavailable on Windows")
	}
	var gotAuth atomic.Value
	path := serveOnUnixSocket(t, 0o700, 0o600, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		jsonHandler(`{"ok":true}`)(w, r)
	}))
	tr := transportFor(t, "unix://"+path, "")
	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/api/v1/health", nil, nil, nil, &out, http.StatusOK)
	require.NoError(t, err)
	assert.True(t, out.OK)
	assert.Empty(t, gotAuth.Load(), "a socket relies on its owner, not a bearer key")
}

// Kata's local daemon creates its socket inside a 0700 directory with a mode
// taken from the umask: 0775 under the common umask 002.
func TestTransportAcceptsAGroupWritableSocketInAPrivateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket ownership checks are unavailable on Windows")
	}
	path := serveOnUnixSocket(t, 0o700, 0o775, jsonHandler(`{"ok":true}`))
	tr := transportFor(t, "unix://"+path, "")
	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
	require.NoError(t, err)
}

func TestTransportRefusesInsecureUnixSockets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket ownership checks are unavailable on Windows")
	}
	handler := jsonHandler(`{"ok":true}`)
	t.Run("world accessible directory", func(t *testing.T) {
		path := serveOnUnixSocket(t, 0o755, 0o600, handler)
		_, err := newTransportForSocket(t, path)
		require.ErrorIs(t, err, ErrInsecureEndpoint)
	})
	t.Run("symlinked socket", func(t *testing.T) {
		path := serveOnUnixSocket(t, 0o700, 0o600, handler)
		link := filepath.Join(filepath.Dir(path), "link.sock")
		require.NoError(t, os.Symlink(path, link))
		_, err := newTransportForSocket(t, link)
		require.ErrorIs(t, err, ErrInsecureEndpoint)
	})
	t.Run("not a socket", func(t *testing.T) {
		dir := shortTempDir(t)
		require.NoError(t, os.Chmod(dir, 0o700))
		file := filepath.Join(dir, "plain")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		_, err := newTransportForSocket(t, file)
		require.ErrorIs(t, err, ErrInsecureEndpoint)
	})
	t.Run("missing socket", func(t *testing.T) {
		_, err := newTransportForSocket(t, filepath.Join(shortTempDir(t), "gone.sock"))
		require.ErrorIs(t, err, ErrUnreachable)
	})
}

func newTransportForSocket(t *testing.T, path string) (*transport, error) {
	t.Helper()
	endpoint, err := config.ParseKataEndpoint("unix://" + path)
	require.NoError(t, err)
	return newTransport(transportOptions{Endpoint: endpoint})
}

func TestValidatePathSegment(t *testing.T) {
	for _, ok := range []string{"7", "abc4", "01J0ZZZZZZZZZZZZZZZZZZZZZZ", "a.b", "a%2Fb", "name:proj"} {
		require.NoError(t, validatePathSegment(ok), ok)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a?b", "a#b", "../x"} {
		assert.ErrorIs(t, validatePathSegment(bad), ErrRequestRejected, bad)
	}
}

func FuzzBuildURLKeepsSegmentsIntact(f *testing.F) {
	for _, seed := range []string{"7", "a%2Fb", "a b", "é", "a:b", "x+y", "100%", "a%zz", "%00"} {
		f.Add(seed)
	}
	base, err := url.Parse("https://kata.example.com/proxy/")
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, segment string) {
		if validatePathSegment(segment) != nil {
			return
		}
		built := buildURL(base, "/api/v1/projects/7/issues/"+segment, url.Values{"q": {segment}})
		parsed, err := url.Parse(built)
		require.NoError(t, err, "%q", built)
		require.Equal(t, "/proxy/api/v1/projects/7/issues/"+segment, parsed.Path, "%q", segment)
		require.Equal(t, segment, parsed.Query().Get("q"))
		require.Equal(t, "kata.example.com", parsed.Host)
	})
}

// Error text from a server ends up in daemon logs, so a hostile or buggy
// server must not be able to put control characters, such as newlines that
// forge log lines or escape sequences that rewrite a terminal, into it.
func TestTransportStripsControlCharactersFromServerText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad\u001b[2Jcode\n","message":"line one\r\nline two\u0000\u0085\u009b end\ttab"}}`))
	}))
	defer srv.Close()
	tr := transportFor(t, srv.URL, "k")
	var out pong
	_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
	require.ErrorIs(t, err, ErrRequestRejected)
	assert.Negative(t, strings.IndexFunc(err.Error(), unicode.IsControl), "%q", err.Error())
	assert.Equal(t, "bad[2Jcode", ErrorCode(err))
	apiErr, ok := AsAPIError(err)
	require.True(t, ok)
	assert.Equal(t, "line oneline two endtab", apiErr.Message)
}

func FuzzSanitizeServerText(f *testing.F) {
	for _, seed := range []string{"", "plain", "a\nb", "\x1b[31mred", "\u0085\u009b", "é\x00ü", strings.Repeat("x", 500), strings.Repeat("\n", 500) + "end", "\xff\xfe"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got := sanitizeServerText(text)
		require.Negative(t, strings.IndexFunc(got, unicode.IsControl), "%q -> %q", text, got)
	})
}
