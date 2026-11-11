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
	for _, tc := range []struct{ name, key, authorization string }{
		{"keyed HTTP", "static-credential", "Bearer static-credential"},
		{"keyless HTTPS", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got http.Header
			var gotPath, gotQuery string
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, gotPath, gotQuery = r.Header.Clone(), r.URL.Path, r.URL.RawQuery
				jsonHandler(`{"ok":true}`)(w, r)
			}))
			if tc.key == "" {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			// Keyless HTTPS may rely on mutual TLS or an authenticating proxy.
			tr := transportFor(t, srv.URL+"/proxy/", tc.key, func(o *transportOptions) { o.HTTPClient = srv.Client() })
			var out pong
			_, err := tr.do(t.Context(), http.MethodGet, "/api/v1/health", url.Values{"a": {"b c"}}, nil, nil, &out, http.StatusOK)
			require.NoError(t, err)
			assert := assert.New(t)
			assert.True(out.OK)
			assert.Equal(tc.authorization, got.Get("Authorization"))
			assert.Equal("application/json", got.Get("Accept"))
			assert.Equal("/proxy/api/v1/health", gotPath, "a path prefix on the endpoint is kept")
			assert.Equal("a=b+c", gotQuery)
		})
	}
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
	done    <-chan struct{}
	started chan struct{}
}

func (b *cancellationBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.done
	return 0, io.ErrUnexpectedEOF
}

func (*cancellationBody) Close() error { return nil }

func TestTransportPreservesCallerCancellationWhileReadingBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &cancellationBody{done: r.Context().Done(), started: started},
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

func TestTransportAllowsManyIndividuallyBoundedRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			select {
			case <-time.After(4 * time.Second):
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})}
		tr := transportFor(t, "https://kata.example.com", "k", func(o *transportOptions) { o.HTTPClient = client })
		start := time.Now()
		for range 8 {
			var out pong
			_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
			require.NoError(t, err)
			assert.True(t, out.OK)
		}
		assert.Equal(t, 32*time.Second, time.Since(start))
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

func TestTransportReportsKataErrorCodesAndMessages(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		code, message string
		want          error
	}{
		{"retry conflict", http.StatusConflict, "idempotency_mismatch", "key reused with a different body", ErrConflict},
		{"complete long message", http.StatusBadRequest, "bad", strings.Repeat("m", (64<<10)+1), ErrRequestRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"code":"` + tc.code + `","message":"` + tc.message + `"}}`))
			}))
			defer srv.Close()
			tr := transportFor(t, srv.URL, "k")
			var out pong
			_, err := tr.do(t.Context(), http.MethodPost, "/x", nil, nil, nil, &out, http.StatusOK)
			require.ErrorIs(t, err, tc.want)
			apiErr, ok := AsAPIError(err)
			require.True(t, ok)
			assert := assert.New(t)
			assert.Equal(tc.code, ErrorCode(err))
			assert.Equal(tc.status, apiErr.Status)
			assert.Equal(tc.message, apiErr.Message)
			assert.Contains(apiErr.Error(), tc.code)
		})
	}
	assert.Empty(t, ErrorCode(ErrRedirect))
}

func TestTransportDecodesSuccessResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"known fields", `{"ok":true}`, true},
		{"additive fields", `{"ok":true,"later_field":{"x":1}}`, true},
		{"not json", `<html>`, false},
		{"trailing data", `{"ok":true} {"ok":false}`, false},
		{"duplicate fields", `{"ok":true,"ok":false}`, false},
		{"wrong type", `{"ok":"yes"}`, false},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(jsonHandler(tc.body))
			defer srv.Close()
			tr := transportFor(t, srv.URL, "k")
			var out pong
			_, err := tr.do(t.Context(), http.MethodGet, "/x", nil, nil, nil, &out, http.StatusOK)
			if !tc.valid {
				require.ErrorIs(t, err, ErrInvalidResponse)
				return
			}
			require.NoError(t, err)
			assert.True(t, out.OK)
		})
	}
}

func TestTransportReportsAClosedPortAsUnreachable(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
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
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, socketMode))
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

func TestTransportUnixSocketSecurity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket ownership checks are unavailable on Windows")
	}
	for _, tc := range []struct {
		name                string
		dirMode, socketMode os.FileMode
		want                error
	}{
		{"private socket", 0o700, 0o600, nil},
		// Kata's daemon uses its umask: a common 002 produces a 0775 socket.
		{"group writable socket in private directory", 0o700, 0o775, nil},
		{"world accessible directory", 0o755, 0o600, ErrInsecureEndpoint},
		{"symlinked socket", 0o700, 0o600, ErrInsecureEndpoint},
		{"not a socket", 0o700, 0o600, ErrInsecureEndpoint},
		{"missing socket", 0o700, 0o600, ErrUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth atomic.Value
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth.Store(r.Header.Get("Authorization"))
				jsonHandler(`{"ok":true}`)(w, r)
			})
			var path string
			switch tc.name {
			case "not a socket", "missing socket":
				dir := shortTempDir(t)
				require.NoError(t, os.Chmod(dir, tc.dirMode))
				path = filepath.Join(dir, "plain")
				if tc.name == "not a socket" {
					require.NoError(t, os.WriteFile(path, nil, 0o600))
				}
			default:
				path = serveOnUnixSocket(t, tc.dirMode, tc.socketMode, handler)
				if tc.name == "symlinked socket" {
					link := filepath.Join(filepath.Dir(path), "link.sock")
					require.NoError(t, os.Symlink(path, link))
					path = link
				}
			}
			tr, err := newTransportForSocket(t, path)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			var out pong
			_, err = tr.do(t.Context(), http.MethodGet, "/api/v1/health", nil, nil, nil, &out, http.StatusOK)
			require.NoError(t, err)
			assert.True(t, out.OK)
			assert.Empty(t, gotAuth.Load(), "a socket relies on its owner, not a bearer key")
		})
	}
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
		var want strings.Builder
		for _, r := range text {
			if !unicode.IsControl(r) {
				want.WriteRune(r)
			}
		}
		require.Equal(t, want.String(), sanitizeServerText(text))
	})
}

// HTTP header values allow tab and bytes above ASCII, but reject other C0
// controls and DEL. Check both classifications and exact delivery without
// imposing an extra token grammar on an otherwise usable credential.
func FuzzTransportCredentialHeader(f *testing.F) {
	for _, seed := range []string{"", "opaque:key", "tab\tkey", "é", "\xff", "bad\nkey", "bad\x00key", "bad\x7fkey"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, content string) {
		key := "prefix" + content + "suffix"
		var sent []string
		endpoint, err := config.ParseKataEndpoint("https://kata.example.com")
		require.NoError(t, err)
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent = append(sent, r.Header.Get("Authorization"))
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
		})}
		tr, err := newTransport(transportOptions{Endpoint: endpoint, APIKey: key, HTTPClient: client})
		valid := true
		for i := range len(content) {
			if content[i] < 0x20 && content[i] != '\t' || content[i] == 0x7f {
				valid = false
			}
		}
		if !valid {
			require.ErrorIs(t, err, ErrAuthenticationRequired)
			require.Equal(t, ErrAuthenticationRequired.Error(), err.Error(), "invalid credentials are never echoed")
			require.Empty(t, sent)
			return
		}
		require.NoError(t, err)
		t.Cleanup(tr.closeIdleConnections)
		var out pong
		_, err = tr.do(t.Context(), http.MethodGet, "/api/v1/health", nil, nil, nil, &out, http.StatusOK)
		require.NoError(t, err)
		require.Equal(t, []string{"Bearer " + key}, sent)
	})
}
