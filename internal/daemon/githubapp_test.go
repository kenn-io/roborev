package daemon

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAppID          = 12345
	testInstallationID = 67890
)

// sharedTestKey generates one 2048-bit RSA key for the entire test
// binary. Key generation is ~0.5-1s per call, and most tests only
// need a valid key — not a unique one.
var (
	sharedKey     *rsa.PrivateKey
	sharedKeyPEM  string
	sharedKeyErr  error
	sharedKeyOnce sync.Once
)

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	sharedKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			sharedKeyErr = err
			return
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
		sharedKey = key
		sharedKeyPEM = string(pemBytes)
	})
	if sharedKeyErr != nil {
		require.Condition(t, func() bool {
			return false
		}, "generate test key: %v", sharedKeyErr)
	}
	return sharedKey, sharedKeyPEM
}

func generateTestKeyPKCS8(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	// PKCS8 parse test needs a PKCS8-encoded key specifically,
	// so generate a fresh one (only called once).
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "marshal PKCS8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})
	return key, string(pemBytes)
}

func setupMockProvider(t *testing.T, handler http.HandlerFunc) (*GitHubAppTokenProvider, *httptest.Server) {
	t.Helper()
	_, pemData := testKey(t)
	tp, err := NewGitHubAppTokenProvider(testAppID, pemData)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "new provider: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	tp.baseURL = srv.URL
	return tp, srv
}

func parseInsecureJWT(t *testing.T, token string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		require.Condition(t, func() bool {
			return false
		}, "expected 3 JWT parts, got %d", len(parts))
	}

	headerBytes, err := base64URLDecode(parts[0])
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "decode header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "parse header: %v", err)
	}

	payloadBytes, err := base64URLDecode(parts[1])
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "decode payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "parse payload: %v", err)
	}
	return header, payload
}

func TestParsePrivateKey_PKCS1(t *testing.T) {
	t.Parallel()
	_, pemData := testKey(t)
	key, err := parsePrivateKey([]byte(pemData))
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "parse PKCS1: %v", err)
	}
	if key == nil {
		require.Condition(t, func() bool {
			return false
		}, "expected non-nil key")
	}
}

func TestParsePrivateKey_PKCS8(t *testing.T) {
	t.Parallel()
	_, pemData := generateTestKeyPKCS8(t)
	key, err := parsePrivateKey([]byte(pemData))
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "parse PKCS8: %v", err)
	}
	if key == nil {
		require.Condition(t, func() bool {
			return false
		}, "expected non-nil key")
	}
}

func TestParsePrivateKey_Invalid(t *testing.T) {
	_, err := parsePrivateKey([]byte("not a PEM"))
	if err == nil {
		require.Condition(t, func() bool {
			return false
		}, "expected error for invalid PEM")
	}
	if !strings.Contains(err.Error(), "no PEM block") {
		require.Condition(t, func() bool {
			return false
		}, "unexpected error: %v", err)
	}
}

func TestSignJWT_Structure(t *testing.T) {
	_, pemData := testKey(t)
	tp, err := NewGitHubAppTokenProvider(testAppID, pemData)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "new provider: %v", err)
	}

	jwt, err := tp.signJWT()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "sign JWT: %v", err)
	}

	header, payload := parseInsecureJWT(t, jwt)

	if header["alg"] != "RS256" {
		assert.Condition(t, func() bool {
			return false
		}, "expected alg RS256, got %v", header["alg"])
	}
	if header["typ"] != "JWT" {
		assert.Condition(t, func() bool {
			return false
		}, "expected typ JWT, got %v", header["typ"])
	}

	iss, ok := payload["iss"].(float64)
	if !ok || int64(iss) != testAppID {
		assert.Condition(t, func() bool {
			return false
		}, "expected iss=%d, got %v", testAppID, payload["iss"])
	}

	iat, ok := payload["iat"].(float64)
	if !ok {
		require.Condition(t, func() bool {
			return false
		}, "missing iat")
	}
	exp, ok := payload["exp"].(float64)
	if !ok {
		require.Condition(t, func() bool {
			return false

			// assert relative invariants to avoid flaky tests under slow execution
		}, "missing exp")
	}

	if exp-iat != 660 {
		assert.Condition(t, func() bool {
			return false
		}, "expected duration (exp-iat) to be 660s, got %v", exp-iat)
	}

	now := float64(time.Now().Unix())
	if iat > now+60 || iat < now-600 {
		assert.Condition(t, func() bool {
			return false
		}, "iat %v out of bounds (now: %v)", iat, now)
	}
	if exp > now+660 || exp < now+60 {
		assert.Condition(t, func() bool {
			return false
		}, "exp %v out of bounds (now: %v)", exp, now)
	}
}

type mockResponse struct {
	path string
	body map[string]any
}

type mockServer struct {
	callCount int
	responses []mockResponse
	t         *testing.T
}

func (m *mockServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.callCount++
	if !assert.LessOrEqual(m.t, m.callCount, len(m.responses), "unexpected call %d", m.callCount) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp := m.responses[m.callCount-1]

	// Verify Authorization header has Bearer JWT
	auth := r.Header.Get("Authorization")
	assert.True(m.t, strings.HasPrefix(auth, "Bearer "), "expected Bearer auth, got: %s", auth)

	if resp.path != "" {
		assert.Equal(m.t, resp.path, r.URL.Path, "unexpected path")
	}

	// Verify User-Agent is set
	ua := r.Header.Get("User-Agent")
	assert.Equal(m.t, "roborev", ua, "expected User-Agent")

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp.body)
}

func TestTokenCaching(t *testing.T) {
	mock := &mockServer{
		t: t,
		responses: []mockResponse{
			{
				path: fmt.Sprintf("/app/installations/%d/access_tokens", testInstallationID),
				body: map[string]any{
					"token":      "ghs_test_token_123",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	tp, _ := setupMockProvider(t, mock.ServeHTTP)

	// First call should hit the server
	token1, err := tp.TokenForInstallation(testInstallationID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "first TokenForInstallation(): %v", err)
	}
	if token1 != "ghs_test_token_123" {
		assert.Condition(t, func() bool {
			return false
		}, "expected ghs_test_token_123, got %s", token1)
	}
	if mock.callCount != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "expected 1 server call, got %d", mock.callCount)
	}

	// Second call should use cache
	token2, err := tp.TokenForInstallation(testInstallationID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "second TokenForInstallation(): %v", err)
	}
	if token2 != token1 {
		assert.Condition(t, func() bool {
			return false
		}, "expected cached token")
	}
	if mock.callCount != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "expected still 1 server call (cached), got %d", mock.callCount)
	}
}

func TestTokenRefreshOnExpiry(t *testing.T) {
	mock := &mockServer{
		t: t,
		responses: []mockResponse{
			{
				path: fmt.Sprintf("/app/installations/%d/access_tokens", testInstallationID),
				body: map[string]any{
					"token":      "ghs_old",
					"expires_at": time.Now().Add(1 * time.Second).Format(time.RFC3339),
				},
			},
			{
				path: fmt.Sprintf("/app/installations/%d/access_tokens", testInstallationID),
				body: map[string]any{
					"token":      "ghs_refreshed",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	tp, _ := setupMockProvider(t, mock.ServeHTTP)

	// First call caches expiring token
	token1, err := tp.TokenForInstallation(testInstallationID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "first TokenForInstallation(): %v", err)
	}
	if token1 != "ghs_old" {
		assert.Condition(t, func() bool {
			return false
		}, "expected expiring token, got %s", token1)
	}

	// Second call should refresh since token is within 5 min buffer
	token2, err := tp.TokenForInstallation(testInstallationID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "second TokenForInstallation(): %v", err)
	}
	if token2 != "ghs_refreshed" {
		assert.Condition(t, func() bool {
			return false
		}, "expected refreshed token, got %s", token2)
	}
	if mock.callCount != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "expected 2 server calls (refresh), got %d", mock.callCount)
	}
}

func TestTokenExchangeError(t *testing.T) {
	tp, _ := setupMockProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	_, err := tp.TokenForInstallation(testInstallationID)
	if err == nil {
		require.Condition(t, func() bool {
			return false
		}, "expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		assert.Condition(t, func() bool {
			return false
		}, "expected 401 in error, got: %v", err)
	}
}

func TestTokenCaching_MultipleInstallations(t *testing.T) {
	mock := &mockServer{
		t: t,
		responses: []mockResponse{
			{
				path: "/app/installations/111/access_tokens",
				body: map[string]any{
					"token":      "ghs_token_for_111",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				},
			},
			{
				path: "/app/installations/222/access_tokens",
				body: map[string]any{
					"token":      "ghs_token_for_222",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	tp, _ := setupMockProvider(t, mock.ServeHTTP)

	// Get token for installation 111
	token1, err := tp.TokenForInstallation(111)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "TokenForInstallation(111): %v", err)
	}
	if token1 != "ghs_token_for_111" {
		assert.Condition(t, func() bool {
			return false
		}, "expected ghs_token_for_111, got %s", token1)
	}
	if mock.callCount != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "expected 1 server call, got %d", mock.callCount)
	}

	// Get token for installation 222
	token2, err := tp.TokenForInstallation(222)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "TokenForInstallation(222): %v", err)
	}
	if token2 != "ghs_token_for_222" {
		assert.Condition(t, func() bool {
			return false
		}, "expected ghs_token_for_222, got %s", token2)
	}
	if mock.callCount != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "expected 2 server calls, got %d", mock.callCount)
	}

	// Re-request installation 111 — should be cached
	token1b, err := tp.TokenForInstallation(111)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "TokenForInstallation(111) cached: %v", err)
	}
	if token1b != "ghs_token_for_111" {
		assert.Condition(t, func() bool {
			return false
		}, "expected cached ghs_token_for_111, got %s", token1b)
	}
	if mock.callCount != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "expected still 2 server calls (cached), got %d", mock.callCount)
	}

	// Re-request installation 222 — should be cached
	token2b, err := tp.TokenForInstallation(222)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "TokenForInstallation(222) cached: %v", err)
	}
	if token2b != "ghs_token_for_222" {
		assert.Condition(t, func() bool {
			return false
		}, "expected cached ghs_token_for_222, got %s", token2b)
	}
	if mock.callCount != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "expected still 2 server calls (cached), got %d", mock.callCount)
	}
}
