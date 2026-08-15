package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBrowserPolicy(t *testing.T, auth bool) BrowserPolicy {
	t.Helper()
	endpoint := BrowserEndpoint{
		Address:           "127.0.0.1:7374",
		Origin:            "https://reviews.example.com",
		Enabled:           true,
		remoteAuthEnabled: auth,
	}
	policy, err := NewBrowserPolicy(endpoint, "http://127.0.0.1:5173")
	require.NoError(t, err)
	return policy
}

func TestBrowserPolicyValidatesExactHostAndOrigin(t *testing.T) {
	policy := testBrowserPolicy(t, true)
	for _, host := range []string{"127.0.0.1:7374", "reviews.example.com", "127.0.0.1:5173", "REVIEWS.EXAMPLE.COM"} {
		req := httptest.NewRequest("GET", "http://example.invalid/", nil)
		req.Host = host
		require.NoError(t, policy.ValidateHost(req), host)
	}
	for _, host := range []string{"", "reviews.example.com.attacker.test", "user@reviews.example.com", "reviews.example.com:bad"} {
		req := httptest.NewRequest("GET", "http://example.invalid/", nil)
		req.Host = host
		require.Error(t, policy.ValidateHost(req), host)
	}

	req := httptest.NewRequest("POST", "http://example.invalid/", nil)
	req.Header.Set("Origin", "https://reviews.example.com")
	require.NoError(t, policy.ValidateOrigin(req))
	for _, origin := range []string{"", "null", "https://reviews.example.com.attacker.test", "https://reviews.example.com/path"} {
		req.Header.Set("Origin", origin)
		require.Error(t, policy.ValidateOrigin(req), origin)
	}
}

func TestBrowserPolicyLocalSessionRequiresEveryTrustCondition(t *testing.T) {
	policy := testBrowserPolicy(t, false)
	request := httptest.NewRequest("POST", "http://127.0.0.1:7374/api/ui/session/bootstrap", nil)
	request.Host = "127.0.0.1:7374"
	request.RemoteAddr = "127.0.0.1:50000"
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	assert.True(t, policy.AllowsLocalSession(request))
	request.Header.Del("Origin")
	assert.True(t, policy.AllowsLocalSession(request))
	request.Header.Set("Origin", "http://127.0.0.1:5173")

	mutations := []func(*httpRequest){
		func(req *httpRequest) { req.RemoteAddr = "192.0.2.1:50000" },
		func(req *httpRequest) { req.Host = "reviews.example.com" },
		func(req *httpRequest) { req.Header.Set("Origin", "https://reviews.example.com") },
		func(req *httpRequest) { req.Header.Set("Origin", "null") },
		func(req *httpRequest) { req.Header.Set("X-Forwarded-For", "192.0.2.1") },
	}
	for _, mutate := range mutations {
		req := httptest.NewRequest("POST", "http://127.0.0.1:7374/", nil)
		req.Host = request.Host
		req.RemoteAddr = request.RemoteAddr
		req.Header = request.Header.Clone()
		mutate((*httpRequest)(req))
		assert.False(t, policy.AllowsLocalSession(req))
	}
	assert.False(t, testBrowserPolicy(t, true).AllowsLocalSession(request))
}

type httpRequest = http.Request
