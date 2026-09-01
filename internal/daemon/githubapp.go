package daemon

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// cachedToken holds a cached installation access token with its expiry.
type cachedToken struct {
	token   string
	expires time.Time
}

// GitHubAppTokenProvider obtains GitHub installation access tokens
// using GitHub App JWT authentication. It caches tokens per installation
// and refreshes them when within 5 minutes of expiry. Thread-safe.
type GitHubAppTokenProvider struct {
	appID int64
	key   *rsa.PrivateKey

	// baseURL is the GitHub REST API base URL. Empty uses public GitHub.
	baseURL string

	mu     sync.Mutex
	tokens map[int64]*cachedToken // installation_id → cached token
}

// NewGitHubAppTokenProvider creates a token provider from the given PEM data.
// Supports both PKCS1 and PKCS8 private key formats.
func NewGitHubAppTokenProvider(appID int64, pemData string) (*GitHubAppTokenProvider, error) {
	key, err := parsePrivateKey([]byte(pemData))
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return &GitHubAppTokenProvider{
		appID:  appID,
		key:    key,
		tokens: make(map[int64]*cachedToken),
	}, nil
}

// TokenForInstallation returns a valid access token for the given installation,
// refreshing if needed.
func (p *GitHubAppTokenProvider) TokenForInstallation(installationID int64) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Return cached token if still valid (with 5 minute buffer)
	if ct, ok := p.tokens[installationID]; ok {
		if time.Now().Before(ct.expires.Add(-5 * time.Minute)) {
			return ct.token, nil
		}
	}

	jwt, err := p.signJWT()
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	token, expires, err := p.exchangeToken(jwt, installationID)
	if err != nil {
		return "", fmt.Errorf("exchange token: %w", err)
	}

	p.tokens[installationID] = &cachedToken{token: token, expires: expires}
	return token, nil
}

// signJWT creates an RS256-signed JWT for GitHub App authentication.
func (p *GitHubAppTokenProvider) signJWT() (string, error) {
	now := time.Now()
	header := base64URLEncode([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64URLEncode(fmt.Appendf(nil,
		`{"iss":%d,"iat":%d,"exp":%d}`,
		p.appID, now.Add(-60*time.Second).Unix(), now.Add(10*time.Minute).Unix(),
	))

	sigInput := header + "." + payload
	h := sha256.Sum256([]byte(sigInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}

	return sigInput + "." + base64URLEncode(sig), nil
}

// installationTokenResponse is the response from POST /app/installations/{id}/access_tokens
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// exchangeToken exchanges a JWT for an installation access token.
func (p *GitHubAppTokenProvider) exchangeToken(jwt string, installationID int64) (string, time.Time, error) {
	baseURL := p.baseURL
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", baseURL, installationID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "roborev")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, err
	}

	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, body)
	}

	var result installationTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("parse response: %w", err)
	}
	return result.Token, result.ExpiresAt, nil
}

// parsePrivateKey parses a PEM-encoded RSA private key (PKCS1 or PKCS8).
func parsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	// Try PKCS1 first (RSA PRIVATE KEY)
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	// Try PKCS8 (PRIVATE KEY)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse as PKCS1 or PKCS8: %w", err)
	}

	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PKCS8 key is not RSA")
	}
	return rsaKey, nil
}

// base64URLEncode encodes data as base64url without padding.
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// base64URLDecode decodes base64url data (with or without padding).
func base64URLDecode(s string) ([]byte, error) {
	// RawURLEncoding handles no-padding; also handle padded input
	return base64.RawURLEncoding.DecodeString(s)
}
