package daemon

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	configpkg "go.kenn.io/roborev/internal/config"
)

const (
	WebSessionHeader = "X-Roborev-Web-Session"
	WebCSRFHeader    = "X-Roborev-CSRF"
	defaultWebTTL    = 12 * time.Hour
)

var (
	ErrInvalidWebToken         = errors.New("invalid web token")
	ErrWebSessionRequired      = errors.New("web session required")
	ErrInvalidWebCSRF          = errors.New("invalid web CSRF token")
	ErrLocalWebSessionDisabled = errors.New("local web sessions are disabled")
	ErrWebLoginRateLimited     = errors.New("web login rate limited")
)

type WebLoginRateLimitError struct {
	RetryAfter time.Duration
}

func (e *WebLoginRateLimitError) Error() string { return ErrWebLoginRateLimited.Error() }
func (e *WebLoginRateLimitError) Is(target error) bool {
	return target == ErrWebLoginRateLimited
}

type BrowserSessionConfig struct {
	Origin     string
	AuthToken  string
	AllowLocal bool
	TTL        time.Duration
	Entropy    io.Reader
	Clock      func() time.Time
}

type BrowserPrincipal struct {
	Local bool
}

type SessionCredentials struct {
	Ambient   string
	Tab       string
	CSRF      string
	Expires   time.Time
	Principal BrowserPrincipal
}

type ambientSession struct {
	id        [32]byte
	principal BrowserPrincipal
	expiresAt time.Time
	revoked   chan struct{}
}

type tabSession struct {
	ambientID [32]byte
	csrfHash  [32]byte
	expiresAt time.Time
}

type BrowserSessionManager struct {
	mu                sync.Mutex
	authHash          [32]byte
	authSet           bool
	allowLocal        bool
	ttl               time.Duration
	entropy           io.Reader
	clock             func() time.Time
	cookieName        string
	secure            bool
	ambient           map[[32]byte]ambientSession
	tabs              map[[32]byte]tabSession
	loginFailures     int
	loginBlockedUntil time.Time
}

func NewBrowserSessionManager(config BrowserSessionConfig) (*BrowserSessionManager, error) {
	if config.Entropy == nil {
		return nil, fmt.Errorf("browser session entropy is required")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.TTL <= 0 {
		config.TTL = defaultWebTTL
	}
	if config.AuthToken != "" {
		if err := configpkg.ValidateWebAuthToken(config.AuthToken); err != nil {
			return nil, err
		}
	}
	origin, err := url.Parse(config.Origin)
	if err != nil || origin.Scheme == "" || origin.Host == "" {
		return nil, fmt.Errorf("browser session origin is invalid")
	}
	instance, err := randomToken(config.Entropy, 16)
	if err != nil {
		return nil, fmt.Errorf("create browser session instance: %w", err)
	}
	return &BrowserSessionManager{
		authHash:   sha256.Sum256([]byte(config.AuthToken)),
		authSet:    config.AuthToken != "",
		allowLocal: config.AllowLocal,
		ttl:        config.TTL,
		entropy:    config.Entropy,
		clock:      config.Clock,
		cookieName: "roborev_web_" + instance[:16],
		secure:     origin.Scheme == "https",
		ambient:    make(map[[32]byte]ambientSession),
		tabs:       make(map[[32]byte]tabSession),
	}, nil
}

func (m *BrowserSessionManager) Login(token string) (SessionCredentials, error) {
	m.mu.Lock()
	now := m.clock()
	presented := sha256.Sum256([]byte(token))
	if m.authSet && subtle.ConstantTimeCompare(presented[:], m.authHash[:]) == 1 {
		m.loginFailures = 0
		m.loginBlockedUntil = time.Time{}
		m.mu.Unlock()
		return m.newSession(BrowserPrincipal{})
	}
	if now.Before(m.loginBlockedUntil) {
		retryAfter := m.loginBlockedUntil.Sub(now)
		m.mu.Unlock()
		return SessionCredentials{}, &WebLoginRateLimitError{RetryAfter: retryAfter}
	}
	m.loginFailures++
	m.loginBlockedUntil = now.Add(webLoginBackoff(m.loginFailures))
	m.mu.Unlock()
	return SessionCredentials{}, ErrInvalidWebToken
}

func webLoginBackoff(failures int) time.Duration {
	delay := time.Second
	for attempt := 1; attempt < failures && delay < time.Minute; attempt++ {
		delay *= 2
	}
	return min(delay, time.Minute)
}

func (m *BrowserSessionManager) NewLocalSession() (SessionCredentials, error) {
	if !m.allowLocal {
		return SessionCredentials{}, ErrLocalWebSessionDisabled
	}
	return m.newSession(BrowserPrincipal{Local: true})
}

func (m *BrowserSessionManager) newSession(principal BrowserPrincipal) (SessionCredentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()

	ambientValue, err := randomToken(m.entropy, 32)
	if err != nil {
		return SessionCredentials{}, err
	}
	ambientID := sha256.Sum256([]byte(ambientValue))
	expires := m.clock().Add(m.ttl)
	m.ambient[ambientID] = ambientSession{
		id: ambientID, principal: principal, expiresAt: expires,
		revoked: make(chan struct{}),
	}
	return m.newTabLocked(ambientValue, ambientID, expires, principal)
}

func (m *BrowserSessionManager) Bootstrap(ambient string) (SessionCredentials, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	ambientID := sha256.Sum256([]byte(ambient))
	session, found := m.ambient[ambientID]
	if !found {
		return SessionCredentials{}, ErrWebSessionRequired
	}
	return m.newTabLocked(ambient, session.id, session.expiresAt, session.principal)
}

func (m *BrowserSessionManager) newTabLocked(
	ambient string,
	ambientID [32]byte,
	expires time.Time,
	principal BrowserPrincipal,
) (SessionCredentials, error) {
	tabValue, err := randomToken(m.entropy, 32)
	if err != nil {
		return SessionCredentials{}, err
	}
	csrfValue, err := randomToken(m.entropy, 32)
	if err != nil {
		return SessionCredentials{}, err
	}
	tabID := sha256.Sum256([]byte(tabValue))
	m.tabs[tabID] = tabSession{
		ambientID: ambientID,
		csrfHash:  sha256.Sum256([]byte(csrfValue)),
		expiresAt: expires,
	}
	return SessionCredentials{
		Ambient: ambient, Tab: tabValue, CSRF: csrfValue, Expires: expires,
		Principal: principal,
	}, nil
}

func (m *BrowserSessionManager) Authenticate(ambient, tab string) (BrowserPrincipal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	ambientID := sha256.Sum256([]byte(ambient))
	tabID := sha256.Sum256([]byte(tab))
	session, ambientFound := m.ambient[ambientID]
	tabRecord, tabFound := m.tabs[tabID]
	if !ambientFound || !tabFound || subtle.ConstantTimeCompare(ambientID[:], tabRecord.ambientID[:]) != 1 {
		return BrowserPrincipal{}, ErrWebSessionRequired
	}
	return session.principal, nil
}

func (m *BrowserSessionManager) SessionExpiry(ambient, tab string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	ambientID := sha256.Sum256([]byte(ambient))
	tabID := sha256.Sum256([]byte(tab))
	session, ambientFound := m.ambient[ambientID]
	tabRecord, tabFound := m.tabs[tabID]
	if !ambientFound || !tabFound || subtle.ConstantTimeCompare(ambientID[:], tabRecord.ambientID[:]) != 1 {
		return time.Time{}, ErrWebSessionRequired
	}
	return session.expiresAt, nil
}

func (m *BrowserSessionManager) CheckCSRF(tab, csrf string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	tabID := sha256.Sum256([]byte(tab))
	presented := sha256.Sum256([]byte(csrf))
	record, found := m.tabs[tabID]
	if !found || subtle.ConstantTimeCompare(presented[:], record.csrfHash[:]) != 1 {
		return ErrInvalidWebCSRF
	}
	return nil
}

func (m *BrowserSessionManager) Logout(ambient string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ambientID := sha256.Sum256([]byte(ambient))
	m.revokeAmbientLocked(ambientID)
}

func (m *BrowserSessionManager) sessionLifetime(
	ambient, tab string,
) (<-chan struct{}, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked()
	ambientID := sha256.Sum256([]byte(ambient))
	tabID := sha256.Sum256([]byte(tab))
	session, ambientFound := m.ambient[ambientID]
	tabRecord, tabFound := m.tabs[tabID]
	if !ambientFound || !tabFound || subtle.ConstantTimeCompare(ambientID[:], tabRecord.ambientID[:]) != 1 {
		return nil, 0, ErrWebSessionRequired
	}
	return session.revoked, session.expiresAt.Sub(m.clock()), nil
}

func (m *BrowserSessionManager) revokeAmbientLocked(ambientID [32]byte) {
	session, found := m.ambient[ambientID]
	if found {
		delete(m.ambient, ambientID)
		close(session.revoked)
	}
	for tabID, record := range m.tabs {
		if subtle.ConstantTimeCompare(ambientID[:], record.ambientID[:]) == 1 {
			delete(m.tabs, tabID)
		}
	}
}

func (m *BrowserSessionManager) Cookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     m.cookieName,
		Value:    value,
		Path:     "/",
		Expires:  m.clock().Add(m.ttl),
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
	}
}

func (m *BrowserSessionManager) ExpiredCookie() *http.Cookie {
	cookie := m.Cookie("")
	cookie.Expires = time.Unix(0, 0).UTC()
	cookie.MaxAge = -1
	return cookie
}

func (m *BrowserSessionManager) CookieName() string { return m.cookieName }

func (m *BrowserSessionManager) cleanupLocked() {
	now := m.clock()
	for id, session := range m.ambient {
		if !now.Before(session.expiresAt) {
			m.revokeAmbientLocked(id)
		}
	}
	for id, tab := range m.tabs {
		_, ambientFound := m.ambient[tab.ambientID]
		if !ambientFound || !now.Before(tab.expiresAt) {
			delete(m.tabs, id)
		}
	}
}

func randomToken(reader io.Reader, size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(reader, data); err != nil {
		return "", fmt.Errorf("read browser session entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
