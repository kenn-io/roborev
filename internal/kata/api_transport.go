package kata

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/roborev/internal/config"
)

const (
	// DefaultRequestTimeout matches Kata v0.19.0's non-streaming request budget
	// (internal/client/client.go:38-40).
	DefaultRequestTimeout = 5 * time.Second
)

// Transport and API errors. Callers test them with errors.Is; the status
// states and the hook log lines are built from them.
var (
	ErrAuthenticationRequired = errors.New("kata: authentication required")
	ErrConflict               = errors.New("kata: conflict")
	ErrIncompatible           = errors.New("kata: server is incompatible")
	// ErrInsecureEndpoint is the config package's endpoint error, so a bad
	// endpoint and an insecure socket classify the same way.
	ErrInsecureEndpoint      = config.ErrKataEndpoint
	ErrInvalidResponse       = errors.New("kata: invalid response")
	ErrNotFound              = errors.New("kata: not found")
	ErrPlatformSecurityLimit = errors.New("kata: platform cannot enforce the required socket security")
	ErrProjectNotFound       = errors.New("kata: project not found")
	ErrRedirect              = errors.New("kata: redirect rejected")
	ErrRequestRejected       = errors.New("kata: request rejected")
	ErrUnreachable           = errors.New("kata: server unreachable")
)

// APIError is a non-success reply from Kata. Class is one of the sentinel
// errors above; Code is Kata's own error code such as idempotency_mismatch.
type APIError struct {
	Status  int
	Code    string
	Message string
	Class   error
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v: HTTP %d", e.Class, e.Status)
	if e.Code != "" {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

func (e *APIError) Unwrap() error { return e.Class }

// AsAPIError returns the Kata reply behind err, if any.
func AsAPIError(err error) (*APIError, bool) {
	return errors.AsType[*APIError](err)
}

// ErrorCode returns Kata's error code for a rejected request, or "".
func ErrorCode(err error) string {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.Code
	}
	return ""
}

// transportOptions configures newTransport. HTTPClient is a seam for tests
// that need a TLS trust store; the transport copies it and overrides its
// timeout and redirect policy.
type transportOptions struct {
	Endpoint   config.KataEndpoint
	APIKey     string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// transport is a JSON client for Kata's API: no redirects, a request deadline,
// and bearer authentication from the key.
// The key stays private to this value.
type transport struct {
	base         *url.URL
	kind         config.KataEndpointKind
	apiKey       string
	http         *http.Client
	securityNote string
}

func newTransport(opts transportOptions) (*transport, error) {
	endpoint := opts.Endpoint
	if endpoint.URL == nil {
		return nil, fmt.Errorf("%w: endpoint is empty", ErrInsecureEndpoint)
	}
	key := strings.TrimSpace(opts.APIKey)
	if endpoint.Kind == config.KataEndpointLoopbackHTTP && key == "" {
		return nil, ErrAuthenticationRequired
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	client := http.Client{}
	if opts.HTTPClient != nil {
		client = *opts.HTTPClient
	}
	client.Timeout = timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrRedirect }

	t := &transport{
		base:   endpoint.URL,
		kind:   endpoint.Kind,
		apiKey: key,
	}
	if endpoint.Kind == config.KataEndpointUnix {
		dial, note, err := unixSocketDialer(endpoint.SocketPath, timeout)
		if err != nil {
			return nil, err
		}
		t.securityNote = note
		client.Transport = &http.Transport{DialContext: dial, DisableKeepAlives: true}
	}
	t.http = &client
	return t, nil
}

func (t *transport) closeIdleConnections() {
	if t != nil && t.http != nil {
		t.http.CloseIdleConnections()
	}
}

// do sends one request and decodes a success reply into out. path is a decoded
// URL path; identifier segments must pass validatePathSegment first.
func (t *transport) do(ctx context.Context, method, path string, query url.Values, body any, header http.Header, out any, success ...int) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body, json.Deterministic(true))
		if err != nil {
			return nil, fmt.Errorf("encode kata request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, buildURL(t.base, path, query), reader)
	if err != nil {
		return nil, fmt.Errorf("build kata request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	resp, err := t.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		switch {
		case errors.Is(err, ErrRedirect):
			return nil, ErrRedirect
		case errors.Is(err, ErrInsecureEndpoint), errors.Is(err, ErrPlatformSecurityLimit):
			return nil, err
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		}
		// The URL error text can name the endpoint and never the key, but the
		// cause adds nothing the caller can act on.
		return nil, fmt.Errorf("%w: request failed", ErrUnreachable)
	}
	defer func() { _ = resp.Body.Close() }()

	if !slices.Contains(success, resp.StatusCode) {
		apiErr := readAPIError(resp)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, apiErr
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: read response", ErrUnreachable)
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(data))
	if err := json.UnmarshalDecode(decoder, out); err != nil {
		return nil, fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	if err := json.UnmarshalDecode(decoder, &struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing response data", ErrInvalidResponse)
	}
	return resp.Header.Clone(), nil
}

// readAPIError classifies a failed reply and keeps Kata's error code and
// complete message from its error envelope when it sent one.
func readAPIError(resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode, Class: classifyStatus(resp.StatusCode)}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: read error response", ErrUnreachable)
	}
	if json.Unmarshal(data, &envelope) == nil {
		apiErr.Code = sanitizeServerText(envelope.Error.Code)
		apiErr.Message = sanitizeServerText(envelope.Error.Message)
	}
	return apiErr
}

// sanitizeServerText removes control characters that could forge a log line
// or drive a terminal. All other text is preserved.
func sanitizeServerText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func classifyStatus(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAuthenticationRequired
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return ErrConflict
	case http.StatusBadRequest, http.StatusNotAcceptable,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity,
		http.StatusRequestEntityTooLarge, http.StatusRequestHeaderFieldsTooLarge:
		return ErrRequestRejected
	}
	if isTransientStatus(status) {
		return ErrUnreachable
	}
	return ErrIncompatible
}

func isTransientStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return status >= http.StatusInternalServerError && status <= 599 && status != http.StatusNotImplemented
}

// buildURL joins base, a decoded path, and an encoded query. Assigning the
// decoded path with an empty RawPath makes URL.String percent-encode it
// exactly once, so a segment such as "a%2Fb" reaches the server unchanged.
func buildURL(base *url.URL, path string, query url.Values) string {
	target := *base
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawPath = ""
	target.RawQuery = query.Encode()
	return target.String()
}

// validatePathSegment rejects identifiers that could change the URL structure
// when embedded as one path segment.
func validatePathSegment(value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#") {
		return fmt.Errorf("%w: invalid path identity", ErrRequestRejected)
	}
	return nil
}

// unixSocketDialer returns a dialer for a private, user-owned Unix socket. The
// path is checked once here and the peer's owner on every connection where
// the platform can report it.
func unixSocketDialer(socketPath string, timeout time.Duration) (dial func(context.Context, string, string) (net.Conn, error), note string, err error) {
	owner := currentUserID()
	if err := validateSecureSocket(socketPath, owner); err != nil {
		return nil, "", err
	}
	if !peerCredentialsSupported() {
		note = "peer credentials unavailable on this platform; socket path and parent ownership and mode are enforced"
	}
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err != nil {
			return nil, fmt.Errorf("%w: connect Unix socket", ErrUnreachable)
		}
		if err := verifyPeerCredentials(conn, owner); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}, note, nil
}
