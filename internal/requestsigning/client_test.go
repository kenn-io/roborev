package requestsigning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type trackedRequestBody struct {
	io.Reader
	closed bool
}

func (b *trackedRequestBody) Close() error { b.closed = true; return nil }

func TestClientClosesBodyBeforeTransportOnSigningFailure(t *testing.T) {
	key := testKey(t)
	for _, failure := range []string{"origin", "prefix", "key", "nonrepeatable"} {
		t.Run(failure, func(t *testing.T) {
			client, err := HTTPClient("https://reviews.example.com/history", nil, func() (Key, error) {
				if failure == "key" {
					return Key{}, errors.New("key unavailable")
				}
				return key, nil
			})
			require.NoError(t, err)
			target := "https://reviews.example.com/history/api/ping"
			switch failure {
			case "origin":
				target = "https://other.example.com/history/api/ping"
			case "prefix":
				target = "https://reviews.example.com/elsewhere"
			}
			body := &trackedRequestBody{Reader: strings.NewReader("{}")}
			r, err := http.NewRequest("GET", target, body)
			require.NoError(t, err)
			r.Header.Set("Authorization", "Bearer synthetic")
			_, err = client.Transport.RoundTrip(r)
			require.Error(t, err)
			assert.True(t, body.closed, "RoundTripper must close bodies on every error path")
		})
	}
}

func TestClientHTTPSOriginPrefixRedirectAndReconnect(t *testing.T) {
	key := testKey(t)
	var nonces []string
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		v, err := VerifyHeaders(r, target, map[string]Key{key.ID: key}, time.Now())
		assert.NoError(t, err)
		assert.NoError(t, VerifyBody(w, r, 1024, t.TempDir()))
		defer r.Body.Close()
		mu.Lock()
		nonces = append(nonces, v.Nonce)
		mu.Unlock()
		if r.URL.Path == "/history/redirect" {
			http.Redirect(w, r, "/history/destination", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	client, err := HTTPClient(server.URL+"/history", server.Client(), func() (Key, error) { return key, nil })
	require.NoError(t, err)
	for range 2 {
		r, err := http.NewRequest("GET", server.URL+"/history/api/ping", nil)
		require.NoError(t, err)
		r.Header.Set("Authorization", "Bearer synthetic")
		resp, err := client.Do(r)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Empty(t, r.Header.Get("Signature"))
	}
	mu.Lock()
	assert.NotEqual(t, nonces[0], nonces[1])
	mu.Unlock()
	r, _ := http.NewRequest("GET", server.URL+"/history/redirect", nil)
	r.Header.Set("Authorization", "Bearer synthetic")
	resp, err := client.Do(r)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 307, resp.StatusCode)
	for _, url := range []string{server.URL + "/elsewhere", server.URL + "/history-other/api/ping", "https://other.example/history/api/ping"} {
		_, err = client.Get(url)
		require.Error(t, err)
	}
	_, err = HTTPClient("http://example.com", nil, func() (Key, error) { return key, nil })
	require.Error(t, err)
}

func TestClientDoesNotRetryAdmittedRequestImplicitly(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", h2), func(t *testing.T) {
			key := testKey(t)
			var admissions atomic.Int64
			type connectionKey struct{}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := VerifyHeaders(r, "https://"+r.Host+r.URL.RequestURI(), map[string]Key{key.ID: key}, time.Now())
				assert.NoError(t, err)
				assert.NoError(t, VerifyBody(w, r, 1024, t.TempDir()))
				defer r.Body.Close()
				if r.URL.Path == "/history/lose" && admissions.Add(1) == 1 {
					_ = r.Context().Value(connectionKey{}).(net.Conn).Close()
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = h2
			server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
				return context.WithValue(ctx, connectionKey{}, conn)
			}
			server.StartTLS()
			defer server.Close()
			client, err := HTTPClient(server.URL+"/history", server.Client(), func() (Key, error) { return key, nil })
			require.NoError(t, err)
			request := func(path string) (*http.Response, error) {
				r, err := http.NewRequest("GET", server.URL+"/history/"+path, nil)
				require.NoError(t, err)
				r.Header.Set("Authorization", "Bearer synthetic")
				return client.Do(r)
			}
			prime, err := request("prime")
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, prime.Body)
			require.NoError(t, err)
			require.NoError(t, prime.Body.Close())
			if h2 {
				assert.Equal(t, 2, prime.ProtoMajor)
			}
			resp, err := request("lose")
			if resp != nil {
				resp.Body.Close()
			}
			require.Error(t, err, "a wire admission must not be resent implicitly")
			assert.Equal(t, int64(1), admissions.Load())
			resp, err = request("lose")
			require.NoError(t, err, "an explicit outer retry gets a fresh signature")
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, int64(2), admissions.Load())
		})
	}
}
