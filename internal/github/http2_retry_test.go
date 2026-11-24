package github

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
)

func TestGitHubReadRetriesHTTP2InterruptedResponse(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts atomic.Int32
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, 2, r.ProtoMajor)
					assert.Equal(t, http.MethodGet, r.Method)
					assert.Equal(t, "/api/v3/repos/acme/api/pulls", r.URL.Path)
					attempt := attempts.Add(1)
					if persistent || attempt == 1 {
						_, err := io.WriteString(w, `[{"number":9},`)
						assert.NoError(t, err)
						assert.NoError(t, http.NewResponseController(w).Flush())
						// Go's HTTP/2 server turns this abort into an INTERNAL_ERROR reset
						// after response headers/data, so the SDK observes a body-read error.
						panic(http.ErrAbortHandler)
					}
					_, err := io.WriteString(w, `[{"number":7}]`)
					assert.NoError(t, err)
				}))
				server.EnableHTTP2 = true
				client, err := NewClient("", WithBaseURL("https://example.com/"), WithHTTPClient(server.Client()))
				require.NoError(t, err)
				prs, err := client.ListOpenPullRequests(t.Context(), "acme/api")
				if persistent {
					require.Error(t, err)
					stream, ok := errors.AsType[http2.StreamError](err)
					require.True(t, ok)
					assert.Equal(t, http2.ErrCodeInternal, stream.Code)
					assert.Equal(t, "list pull requests: network interruption", ReadErrorSummary(err))
					assert.Equal(t, int32(4), attempts.Load())
				} else {
					require.NoError(t, err)
					require.Len(t, prs, 1)
					assert.Equal(t, 7, prs[0].Number)
					assert.Equal(t, int32(2), attempts.Load())
				}
			})
		})
	}
}

func TestGitHubReadHTTP2StreamErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code     http2.ErrCode
		attempts int
		detail   string
	}{
		{http2.ErrCodeCancel, 4, "network interruption"},
		{http2.ErrCodeRefusedStream, 4, "network interruption"},
		{http2.ErrCodeInternal, 4, "network interruption"},
		{http2.ErrCodeProtocol, 1, "invalid response"},
		{http2.ErrCodeCompression, 1, "invalid response"},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				client, err := NewClient("", WithHTTPClient(&http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
					attempts++
					assert.Equal(t, http.MethodGet, r.Method)
					return nil, http2.StreamError{StreamID: 1, Code: tc.code, Cause: errors.New("sensitive peer detail")}
				})}))
				require.NoError(t, err)
				_, err = client.ListOpenPullRequests(t.Context(), "acme/api")
				require.Error(t, err)
				stream, ok := errors.AsType[http2.StreamError](err)
				require.True(t, ok)
				assert.Equal(t, tc.code, stream.Code)
				assert.Equal(t, tc.attempts, attempts)
				assert.Equal(t, "list pull requests: "+tc.detail, ReadErrorSummary(err))
				assert.NotContains(t, err.Error(), "sensitive peer detail")
			})
		})
	}
}

type blackholeConn struct {
	*tls.Conn
	drop *atomic.Bool
}

func (c *blackholeConn) Write(b []byte) (int, error) {
	if c.drop.Load() {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func TestGitHubReadRecoversFromBlackholedHTTP2Connection(t *testing.T) {
	for _, tc := range []struct {
		name           string
		useConditional bool
	}{
		{name: "default client", useConditional: false},
		{name: "conditional client", useConditional: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, 2, r.ProtoMajor)
					n := requests.Add(1)
					_, err := fmt.Fprintf(w, `[{"number":%d}]`, n)
					assert.NoError(t, err)
				}))
				server.EnableHTTP2 = true

				// Install a plain fake-network *http.Transport (no HTTP/2 ping
				// config) as http.DefaultTransport so the test exercises the real
				// DefaultHTTPTransport() production wiring.
				transport := server.Client().Transport.(*http.Transport).Clone()
				origDialTLS := transport.DialTLSContext

				var dials atomic.Int32
				var dropFirst atomic.Bool
				transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					conn, err := origDialTLS(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					tc, ok := conn.(*tls.Conn)
					if !ok {
						return conn, nil
					}
					if err := tc.HandshakeContext(ctx); err != nil {
						tc.Close()
						return nil, err
					}
					if dials.Add(1) == 1 {
						return &blackholeConn{Conn: tc, drop: &dropFirst}, nil
					}
					return tc, nil
				}

				prevDefault := http.DefaultTransport
				http.DefaultTransport = transport
				t.Cleanup(func() { http.DefaultTransport = prevDefault })

				opts := []ClientOption{WithBaseURL("https://example.com/")}
				if tc.useConditional {
					opts = append(opts, WithHTTPClient(NewConditionalHTTPClient()))
				}
				client, err := NewClient("", opts...)
				require.NoError(t, err)

				prs, err := client.ListOpenPullRequests(t.Context(), "acme/api")
				require.NoError(t, err)
				require.Len(t, prs, 1)
				assert.Equal(t, 1, prs[0].Number)
				assert.Equal(t, int32(1), dials.Load())

				// Silently drop all writes on the pooled HTTP/2 connection without closing it.
				dropFirst.Store(true)

				prs, err = client.ListOpenPullRequests(t.Context(), "acme/api")
				require.NoError(t, err)
				require.Len(t, prs, 1)
				assert.Equal(t, 2, prs[0].Number)
				assert.Equal(t, int32(2), dials.Load())
			})
		})
	}
}
