package requestsigning

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPClient signs at the final native transport boundary, after auth injection.
// It pins both origin and prefix and refuses redirects, including same-origin
// ones. Post-send net/http retries are disabled; caller retries must call Do
// again and receive a new nonce. There is no unsigned fallback.
func HTTPClient(baseURL string, client *http.Client, key func() (Key, error)) (*http.Client, error) {
	base, err := ValidateBase(baseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	transport := clone.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	clone.Transport = &signingTransport{base: transport, origin: base, key: key}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone, nil
}

type signingTransport struct {
	base   http.RoundTripper
	origin *url.URL
	key    func() (Key, error)
}

func (t *signingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	handedOff := false
	defer func() {
		// RoundTripper owns the body even when validation fails before sending.
		if !handedOff && r.Body != nil {
			_ = r.Body.Close()
		}
	}()
	if r.URL == nil || r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host || r.Host != "" && r.Host != t.origin.Host || r.URL.User != nil {
		return nil, errors.New("signed request must use its configured HTTPS origin")
	}
	if err := validatePath(r.URL); err != nil {
		return nil, err
	}
	if t.origin.Path != "" && r.URL.Path != t.origin.Path && !strings.HasPrefix(r.URL.Path, t.origin.Path+"/") {
		return nil, errors.New("signed request outside configured prefix")
	}
	k, err := t.key()
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	if err = Sign(clone, k, time.Now()); err != nil {
		return nil, err
	}
	// Go 1.27 treats nil/NoBody as replayable even without GetBody. A distinct
	// non-sentinel empty body prevents post-send HTTP/1 and HTTP/2 retries;
	// unusable-connection retries before any wire write remain safe.
	clone.GetBody = nil
	if clone.Body == nil || clone.Body == http.NoBody {
		clone.Body = io.NopCloser(bytes.NewReader(nil))
	}
	handedOff = true
	return t.base.RoundTrip(clone)
}
