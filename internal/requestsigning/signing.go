// Package requestsigning implements a fixed RFC 9421 native request profile.
// It does not replace transport security or operation authorization.
package requestsigning

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Lifetime and FutureSkew are the explicitly coordinated native wire policy,
// in seconds; they are not limits on existing local API requests.
const (
	Lifetime   int64 = 30
	FutureSkew int64 = 5
	components       = `("@method" "@target-uri" "content-digest" "content-type")`
)

var (
	keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	noncePattern = regexp.MustCompile(`^"[A-Za-z0-9_-]{32}"$`)
	ErrInvalid   = errors.New("invalid or expired request signature")
	// ErrBodyNotAllowed reports a request body on the read-only profile.
	ErrBodyNotAllowed = errors.New("signed requests must not have a body")
	emptyDigest       = "sha-256=:" + base64.StdEncoding.EncodeToString(sha256.New().Sum(nil)) + ":"
)

// Key is an independent signing secret; ID is a public selector.
type Key struct {
	ID     string
	Secret []byte
}

func (k Key) Validate() error {
	if !keyIDPattern.MatchString(k.ID) || len(k.Secret) != 64 {
		return errors.New("signing requires a safe key ID and a 64-byte secret")
	}
	return nil
}

// ValidateBase requires a fixed HTTPS origin with an optional unambiguous prefix.
func ValidateBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("remote base must be an HTTPS origin with an optional path prefix")
	}
	if err = validatePath(u); err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func validatePath(u *url.URL) error {
	p := u.EscapedPath()
	if strings.ContainsAny(p, "%\\\r\n\"") || strings.Contains(p, "//") || strings.HasPrefix(p, "//") {
		return ErrInvalid
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == "." || part == ".." {
			return ErrInvalid
		}
	}
	return nil
}

func validateRequest(r *http.Request) error {
	if r.URL == nil || r.URL.User != nil || r.URL.Fragment != "" || r.URL.Opaque != "" || r.URL.RawFragment != "" {
		return ErrInvalid
	}
	if err := validatePath(r.URL); err != nil {
		return err
	}
	if len(r.Trailer) > 0 || len(r.Header.Values("Trailer")) > 0 || len(r.Header.Values("Content-Encoding")) > 0 {
		return ErrInvalid
	}
	for _, name := range []string{"Content-Type", "Content-Digest", "Signature-Input", "Signature"} {
		if len(r.Header.Values(name)) != 1 || strings.ContainsAny(r.Header.Get(name), "\r\n") || strings.TrimSpace(r.Header.Get(name)) != r.Header.Get(name) {
			return ErrInvalid
		}
	}
	if r.Header.Get("Content-Type") != "application/json" {
		return ErrInvalid
	}
	return nil
}

func params(created int64, nonce, id string) string {
	return fmt.Sprintf(`%s;created=%d;expires=%d;nonce="%s";keyid="%s";alg="hmac-sha256"`, components, created, created+Lifetime, nonce, id)
}

func signatureBase(r *http.Request, target, p string) string {
	return fmt.Sprintf("\"@method\": %s\n\"@target-uri\": %s\n\"content-digest\": %s\n\"content-type\": %s\n\"@signature-params\": %s", r.Method, target, r.Header.Get("Content-Digest"), r.Header.Get("Content-Type"), p)
}

func digest(reader io.Reader) (string, error) {
	h := sha256.New()
	if reader != nil {
		if _, err := io.Copy(h, reader); err != nil {
			return "", err
		}
	}
	return "sha-256=:" + base64.StdEncoding.EncodeToString(h.Sum(nil)) + ":", nil
}

// Sign hashes a repeatable body without buffering it. Request-time signing must
// run on every application retry/reconnect.
func Sign(r *http.Request, k Key, now time.Time) error {
	if err := k.Validate(); err != nil {
		return err
	}
	var body io.ReadCloser
	if r.Body != nil && r.Body != http.NoBody {
		if r.GetBody == nil {
			return errors.New("signed request body must be repeatable")
		}
		var err error
		body, err = r.GetBody()
		if err != nil {
			return errors.New("cannot reopen signing body")
		}
		defer body.Close()
	}
	var reader io.Reader
	if body != nil {
		reader = contextReader{Context: r.Context(), Reader: body}
	}
	d, err := digest(reader)
	if err != nil {
		return errors.New("cannot hash signing body")
	}
	nonce := make([]byte, 24)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	p := params(now.Unix(), base64.RawURLEncoding.EncodeToString(nonce), k.ID)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Digest", d)
	r.Header.Set("Signature-Input", "sig1="+p)
	r.Header.Set("Signature", "sig1=:placeholder:")
	if err = validateRequest(r); err != nil {
		return err
	}
	if r.URL.Scheme != "https" || r.URL.Host == "" || r.Host != "" && r.Host != r.URL.Host {
		return ErrInvalid
	}
	mac := hmac.New(sha256.New, k.Secret)
	_, _ = io.WriteString(mac, signatureBase(r, r.URL.String(), p))
	r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":")
	return nil
}

// Verified identifies the immutable authenticated signature metadata.
type Verified struct {
	KeyID, Nonce     string
	Created, Expires int64
}

func (v Verified) Fresh(now int64) bool {
	return v.Created <= now+FutureSkew && now <= v.Expires && v.Expires == v.Created+Lifetime
}

// parseInput accepts exactly the five canonical SF parameters, retaining their
// transmitted order for @signature-params. Unknown or duplicate fields fail.
func parseInput(input string) (Verified, string, error) {
	fields := strings.SplitN(input, ";", 7)
	if len(fields) != 6 || fields[0] != "sig1="+components {
		return Verified{}, "", ErrInvalid
	}
	v := Verified{}
	seen := map[string]bool{}
	for _, field := range fields[1:] {
		name, value, ok := strings.Cut(field, "=")
		if !ok || seen[name] {
			return Verified{}, "", ErrInvalid
		}
		seen[name] = true
		switch name {
		case "created", "expires":
			// RFC 8941 integers have at most 15 digits. This profile uses
			// nonnegative Unix seconds and canonical decimal serialization.
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || len(value) > 15 || value != strconv.FormatInt(n, 10) {
				return Verified{}, "", ErrInvalid
			}
			if name == "created" {
				v.Created = n
			} else {
				v.Expires = n
			}
		case "nonce":
			if !noncePattern.MatchString(value) {
				return Verified{}, "", ErrInvalid
			}
			v.Nonce = value[1 : len(value)-1]
		case "keyid":
			if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' || !keyIDPattern.MatchString(value[1:len(value)-1]) {
				return Verified{}, "", ErrInvalid
			}
			v.KeyID = value[1 : len(value)-1]
		case "alg":
			if value != `"hmac-sha256"` {
				return Verified{}, "", ErrInvalid
			}
		default:
			return Verified{}, "", ErrInvalid
		}
	}
	return v, strings.TrimPrefix(input, "sig1="), nil
}

// VerifyHeaders authenticates metadata before the server reads any body.
func VerifyHeaders(r *http.Request, target string, keys map[string]Key, now time.Time) (Verified, error) {
	if err := validateRequest(r); err != nil {
		return Verified{}, ErrInvalid
	}
	v, p, err := parseInput(r.Header.Get("Signature-Input"))
	if err != nil || !v.Fresh(now.Unix()) {
		return Verified{}, ErrInvalid
	}
	k, ok := keys[v.KeyID]
	if !ok || k.Validate() != nil {
		return Verified{}, ErrInvalid
	}
	field := r.Header.Get("Signature")
	if !strings.HasPrefix(field, "sig1=:") || !strings.HasSuffix(field, ":") {
		return Verified{}, ErrInvalid
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(strings.TrimPrefix(field, "sig1=:"), ":"))
	if err != nil || len(sig) != sha256.Size || field != "sig1=:"+base64.StdEncoding.EncodeToString(sig)+":" {
		return Verified{}, ErrInvalid
	}
	mac := hmac.New(sha256.New, k.Secret)
	_, _ = io.WriteString(mac, signatureBase(r, target, p))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Verified{}, ErrInvalid
	}
	return v, nil
}

// VerifyEmptyBody accepts only an empty body matching the signed digest. The
// remote listener serves reads only, so it never needs to buffer a body.
func VerifyEmptyBody(r *http.Request) error {
	if r.ContentLength > 0 {
		return ErrBodyNotAllowed
	}
	if r.Header.Get("Content-Digest") != emptyDigest {
		return ErrInvalid
	}
	if r.Body == nil {
		return nil
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r.Body, 1))
	if err != nil {
		return err
	}
	if n != 0 {
		return ErrBodyNotAllowed
	}
	// net/http may populate unannounced trailers only after reading EOF.
	if len(r.Trailer) != 0 {
		return ErrInvalid
	}
	return nil
}

// contextReader checks cancellation between fixed-size hash reads.
type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
