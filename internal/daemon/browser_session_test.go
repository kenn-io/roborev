package daemon

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBrowserAuthToken  = "MDEyMzQ1Njc4OWFiY2RlZmdoaWprbG1ub3BxcnN0dXY"
	testBrowserAuthToken2 = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXowMTIzNDU"
)

func newTestBrowserSessions(t *testing.T, allowLocal bool) (*BrowserSessionManager, *time.Time) {
	t.Helper()
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	manager, err := NewBrowserSessionManager(BrowserSessionConfig{
		Origin:     "https://reviews.example.com",
		AuthToken:  testBrowserAuthToken,
		AllowLocal: allowLocal,
		TTL:        time.Hour,
		Entropy:    bytes.NewReader(bytes.Repeat([]byte("entropy-for-browser-sessions-"), 1000)),
		Clock:      func() time.Time { return now },
	})
	require.NoError(t, err)
	return manager, &now
}

func TestBrowserSessionLoginBootstrapAndAuthentication(t *testing.T) {
	manager, now := newTestBrowserSessions(t, true)
	for attempt, token := range []string{"", "wrong"} {
		_, err := manager.Login(token)
		require.ErrorIs(t, err, ErrInvalidWebToken)
		*now = now.Add(time.Second << attempt)
	}

	first, err := manager.Login(testBrowserAuthToken)
	require.NoError(t, err)
	assert.NotContains(t, first.Ambient+first.Tab+first.CSRF, testBrowserAuthToken)
	principal, err := manager.Authenticate(first.Ambient, first.Tab)
	require.NoError(t, err)
	assert.False(t, principal.Local)
	require.NoError(t, manager.CheckCSRF(first.Tab, first.CSRF))

	second, err := manager.Bootstrap(first.Ambient)
	require.NoError(t, err)
	assert.NotEqual(t, first.Tab, second.Tab)
	assert.NotEqual(t, first.CSRF, second.CSRF)
	_, err = manager.Authenticate(first.Ambient, first.Tab)
	require.NoError(t, err, "bootstrap must not invalidate the first tab")
	_, err = manager.Authenticate(first.Ambient, second.Tab)
	require.NoError(t, err)
	require.ErrorIs(t, manager.CheckCSRF(first.Tab, second.CSRF), ErrInvalidWebCSRF)
}

func TestBrowserSessionLoginBacksOffAfterFailures(t *testing.T) {
	manager, now := newTestBrowserSessions(t, false)

	_, err := manager.Login("wrong")
	require.ErrorIs(t, err, ErrInvalidWebToken)
	_, err = manager.Login(testBrowserAuthToken)
	require.NoError(t, err)

	_, err = manager.Login("wrong-again")
	require.ErrorIs(t, err, ErrInvalidWebToken)
	_, err = manager.Login("wrong-again")
	var limited *WebLoginRateLimitError
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, time.Second, limited.RetryAfter)

	_, err = manager.Login(testBrowserAuthToken)
	require.NoError(t, err, "valid credentials must bypass and reset an invalid-login cooldown")

	_, err = manager.Login("wrong-after-reset")
	require.ErrorIs(t, err, ErrInvalidWebToken)
	*now = now.Add(time.Second)
	_, err = manager.Login("wrong-after-reset")
	require.ErrorIs(t, err, ErrInvalidWebToken)
	_, err = manager.Login("wrong-after-reset")
	require.ErrorAs(t, err, &limited)
	assert.Equal(t, 2*time.Second, limited.RetryAfter)
}

func TestBrowserSessionRejectsCrossSessionAndRestartCredentials(t *testing.T) {
	firstManager, _ := newTestBrowserSessions(t, true)
	secondManager, _ := newTestBrowserSessions(t, true)
	first, err := firstManager.Login(testBrowserAuthToken)
	require.NoError(t, err)
	second, err := firstManager.Login(testBrowserAuthToken)
	require.NoError(t, err)

	_, err = firstManager.Authenticate(first.Ambient, second.Tab)
	require.ErrorIs(t, err, ErrWebSessionRequired)
	_, err = secondManager.Authenticate(first.Ambient, first.Tab)
	require.ErrorIs(t, err, ErrWebSessionRequired)
}

func TestBrowserSessionExpiryLogoutAndLocalPolicy(t *testing.T) {
	manager, now := newTestBrowserSessions(t, true)
	credentials, err := manager.NewLocalSession()
	require.NoError(t, err)
	principal, err := manager.Authenticate(credentials.Ambient, credentials.Tab)
	require.NoError(t, err)
	assert.True(t, principal.Local)

	manager.Logout(credentials.Ambient)
	_, err = manager.Authenticate(credentials.Ambient, credentials.Tab)
	require.ErrorIs(t, err, ErrWebSessionRequired)

	expiring, err := manager.Login(testBrowserAuthToken)
	require.NoError(t, err)
	*now = now.Add(2 * time.Hour)
	_, err = manager.Authenticate(expiring.Ambient, expiring.Tab)
	require.ErrorIs(t, err, ErrWebSessionRequired)

	manager, _ = newTestBrowserSessions(t, false)
	_, err = manager.NewLocalSession()
	require.ErrorIs(t, err, ErrLocalWebSessionDisabled)
}

func TestBrowserSessionProxyPolicyCreatesRemotePrincipal(t *testing.T) {
	manager, err := NewBrowserSessionManager(BrowserSessionConfig{
		Origin:     "https://reviews.example.com",
		AllowProxy: true,
		Entropy:    bytes.NewReader(bytes.Repeat([]byte("entropy-for-browser-sessions-"), 1000)),
		Clock:      time.Now,
	})
	require.NoError(t, err)
	credentials, err := manager.NewProxySession()
	require.NoError(t, err)
	principal, err := manager.Authenticate(credentials.Ambient, credentials.Tab)
	require.NoError(t, err)
	assert.False(t, principal.Local)

	manager, _ = newTestBrowserSessions(t, false)
	_, err = manager.NewProxySession()
	require.ErrorIs(t, err, ErrProxyWebSessionDisabled)
}

func TestBrowserSessionCookieScope(t *testing.T) {
	manager, _ := newTestBrowserSessions(t, true)
	cookie := manager.Cookie("ambient-value")
	assert.True(t, strings.HasPrefix(cookie.Name, "roborev_web_"))
	assert.Equal(t, "ambient-value", cookie.Value)
	assert.Empty(t, cookie.Domain)
	assert.Equal(t, "/", cookie.Path)
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)

	expired := manager.ExpiredCookie()
	assert.Equal(t, cookie.Name, expired.Name)
	assert.Equal(t, -1, expired.MaxAge)
	assert.True(t, expired.Expires.Before(time.Unix(1, 0)))
}

func TestBrowserSessionCookieScopeForBasePath(t *testing.T) {
	manager, err := NewBrowserSessionManager(BrowserSessionConfig{
		Origin:     "https://reviews.example.com",
		AuthToken:  testBrowserAuthToken,
		CookiePath: "/roborev-ci/",
		Entropy:    bytes.NewReader(bytes.Repeat([]byte("entropy-for-browser-sessions-"), 1000)),
		Clock:      time.Now,
	})
	require.NoError(t, err)
	cookie := manager.Cookie("ambient-value")
	assert.Equal(t, "/roborev-ci/", cookie.Path)
	assert.Equal(t, "/roborev-ci/", manager.ExpiredCookie().Path)
}

func TestBrowserSessionConcurrentBootstrapAuthenticateAndLogout(t *testing.T) {
	manager, _ := newTestBrowserSessions(t, true)
	credentials, err := manager.Login(testBrowserAuthToken)
	require.NoError(t, err)

	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			tab, bootstrapErr := manager.Bootstrap(credentials.Ambient)
			if bootstrapErr == nil {
				_, _ = manager.Authenticate(credentials.Ambient, tab.Tab)
			}
		})
	}
	wait.Go(func() {
		manager.Logout(credentials.Ambient)
	})
	wait.Wait()
}
