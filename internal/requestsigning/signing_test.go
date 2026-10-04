package requestsigning

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(t *testing.T) Key {
	t.Helper()
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	return Key{ID: "reader-1", Secret: secret}
}

func signedRequest(t *testing.T, key Key, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequest("POST", "https://example.com/history/api/review?b=2&a=%2F", strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer native-test-auth")
	require.NoError(t, Sign(r, key, time.Now()))
	return r
}

func TestSignVerifyAndTampering(t *testing.T) {
	key := testKey(t)
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"method", func(r *http.Request) { r.Method = "GET" }},
		{"prefix", func(r *http.Request) { r.URL.Path = "/other/api/review" }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "a=%2F&b=2" }},
		{"auth", func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }},
		{"digest", func(r *http.Request) { r.Header.Set("Content-Digest", "sha-256=:AAAA:") }},
		{"type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{"input", func(r *http.Request) {
			r.Header.Set("Signature-Input", strings.ReplaceAll(r.Header.Get("Signature-Input"), "reader-1", "reader-2"))
		}},
		{"signature", func(r *http.Request) { r.Header.Set("Signature", "sig1=:AAAA:") }},
		{"duplicate", func(r *http.Request) { r.Header.Add("Authorization", r.Header.Get("Authorization")) }},
		{"encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
		{"trailer", func(r *http.Request) { r.Trailer = http.Header{"Extra": []string{"data"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedRequest(t, key, "content")
			_, err := VerifyHeaders(r, r.URL.String(), map[string]Key{key.ID: key}, time.Now())
			require.NoError(t, err)
			tc.mutate(r)
			_, err = VerifyHeaders(r, r.URL.String(), map[string]Key{key.ID: key}, time.Now())
			require.Error(t, err)
		})
	}
	r := signedRequest(t, key, "")
	assert.Equal(t, "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:", r.Header.Get("Content-Digest"))
	assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
}

func TestBodyDigestExactAndBounded(t *testing.T) {
	key := testKey(t)
	r := signedRequest(t, key, "body")
	out := httptest.NewRecorder()
	require.NoError(t, VerifyBody(out, r, 4, t.TempDir()))
	got, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.NoError(t, r.Body.Close())
	assert.Equal(t, "body", string(got))
	r = signedRequest(t, key, "body")
	r.Body = io.NopCloser(strings.NewReader("evil"))
	require.Error(t, VerifyBody(out, r, 4, t.TempDir()))
	r = signedRequest(t, key, "body")
	require.Error(t, VerifyBody(out, r, 3, t.TempDir()))
}

func FuzzSignatureInputStrict(f *testing.F) {
	f.Add("reader-1")
	f.Add("escaped\\\"id")
	f.Fuzz(func(t *testing.T, id string) {
		key := Key{ID: id, Secret: bytes.Repeat([]byte{7}, 64)}
		r, _ := http.NewRequest("GET", "https://example.com/api/ping", nil)
		r.Header.Set("Authorization", "Bearer synthetic")
		err := Sign(r, key, time.Unix(1000, 0))
		if err != nil {
			return
		}
		_, err = VerifyHeaders(r, r.URL.String(), map[string]Key{id: key}, time.Unix(1000, 0))
		require.NoError(t, err)
		r.Header.Set("Signature-Input", r.Header.Get("Signature-Input")+" ")
		_, err = VerifyHeaders(r, r.URL.String(), map[string]Key{id: key}, time.Unix(1000, 0))
		require.Error(t, err)
	})
}

func TestSignatureFreshnessAndCanonicalParameters(t *testing.T) {
	key := testKey(t)
	for _, tc := range []struct {
		name   string
		offset time.Duration
		valid  bool
	}{
		{"future allowance", 5 * time.Second, true},
		{"future rejected", 6 * time.Second, false},
		{"expired", -31 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", "https://example.com/api/ping?", nil)
			r.Header.Set("Authorization", "Bearer synthetic")
			now := time.Unix(1000, 0)
			require.NoError(t, Sign(r, key, now.Add(tc.offset)))
			_, err := VerifyHeaders(r, r.URL.String(), map[string]Key{key.ID: key}, now)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			r.URL.ForceQuery = false
			_, err = VerifyHeaders(r, r.URL.String(), map[string]Key{key.ID: key}, now)
			require.Error(t, err)
		})
	}
}

func TestCanonicalTransmittedParameterOrder(t *testing.T) {
	key := testKey(t)
	for _, order := range [][]int{{0, 1, 2, 4, 3}, {4, 2, 0, 3, 1}} {
		r := signedRequest(t, key, "")
		fields := strings.Split(strings.TrimPrefix(r.Header.Get("Signature-Input"), "sig1="), ";")
		p := fields[0]
		for _, i := range order {
			p += ";" + fields[i+1]
		}
		base := fmt.Sprintf("\"@method\": %s\n\"@target-uri\": %s\n\"content-digest\": %s\n\"content-type\": %s\n\"authorization\": %s\n\"@signature-params\": %s", r.Method, r.URL.String(), r.Header.Get("Content-Digest"), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), p)
		mac := hmac.New(sha256.New, key.Secret)
		_, err := io.WriteString(mac, base)
		require.NoError(t, err)
		r.Header.Set("Signature-Input", "sig1="+p)
		r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":")
		_, err = VerifyHeaders(r, r.URL.String(), map[string]Key{key.ID: key}, time.Now())
		require.NoError(t, err)
		for _, suffix := range []string{";created=1", ";unknown=1", " "} {
			r.Header.Set("Signature-Input", "sig1="+p+suffix)
			_, err = VerifyHeaders(r, r.URL.String(), map[string]Key{key.ID: key}, time.Now())
			require.Error(t, err)
		}
	}
}
