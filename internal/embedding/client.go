// Package embedding provides a storage-neutral client for OpenAI-compatible
// embedding endpoints.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kit/vector"
)

// InputKind identifies whether text is indexed content or a search query.
type InputKind string

const (
	InputDocument InputKind = "document"
	InputQuery    InputKind = "query"

	defaultBatchSize = 64
	defaultTimeout   = 30 * time.Second
)

// Config configures an embedding Client.
type Config struct {
	BaseURL             string
	Model               string
	APIKey              string
	Salt                string
	RecipeVersion       int
	Dims                int
	BatchSize           int
	Timeout             time.Duration
	InputTypeMode       string
	TrustPrivateNetwork bool
}

// Client calls an OpenAI-compatible /embeddings endpoint.
type Client struct {
	http          *http.Client
	baseURL       string
	endpoint      string
	model         string
	salt          string
	recipeVersion int
	dims          int
	batchSize     int
	inputTypeMode string
}

// New validates cfg and constructs an origin-pinned embedding client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" || cfg.Dims <= 0 {
		return nil, fmt.Errorf("embedding: base_url, model, and positive dims are required")
	}
	mode := cfg.InputTypeMode
	if mode == "" {
		mode = "none"
	}
	if mode != "none" && mode != "retrieval" {
		return nil, fmt.Errorf("embedding: input_type_mode must be %q or %q", "none", "retrieval")
	}
	batchSize := cfg.BatchSize
	if batchSize == 0 {
		batchSize = defaultBatchSize
	}
	if batchSize < 0 {
		return nil, fmt.Errorf("embedding: batch_size must be positive")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 {
		return nil, fmt.Errorf("embedding: timeout must be positive")
	}

	origin, err := safeEmbeddingOrigin(cfg.BaseURL, cfg.TrustPrivateNetwork)
	if err != nil {
		return nil, err
	}
	parsedBaseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("embedding: invalid base_url")
	}
	endpoint, err := canonicalEmbeddingEndpoint(parsedBaseURL)
	if err != nil {
		return nil, err
	}
	transport := &originPinnedTransport{
		origin:              origin,
		apiKey:              cfg.APIKey,
		trustPrivateNetwork: cfg.TrustPrivateNetwork,
	}
	httpClient := &http.Client{Timeout: timeout, Transport: transport}
	httpClient.CheckRedirect = func(request *http.Request, _ []*http.Request) error {
		redirectOrigin, err := canonicalOrigin(request.URL)
		if err != nil {
			return err
		}
		if redirectOrigin != origin {
			return fmt.Errorf("embedding: cross-origin redirect refused")
		}
		return nil
	}

	return &Client{
		http:          httpClient,
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		endpoint:      endpoint,
		model:         cfg.Model,
		salt:          cfg.Salt,
		recipeVersion: cfg.RecipeVersion,
		dims:          cfg.Dims,
		batchSize:     batchSize,
		inputTypeMode: mode,
	}, nil
}

type originPinnedTransport struct {
	base                http.RoundTripper
	origin              string
	apiKey              string
	trustPrivateNetwork bool
}

func (t *originPinnedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if _, err := safeEmbeddingOrigin(request.URL.String(), t.trustPrivateNetwork); err != nil {
		return nil, err
	}
	origin, err := canonicalOrigin(request.URL)
	if err != nil {
		return nil, err
	}
	if origin != t.origin {
		return nil, fmt.Errorf("embedding: request origin differs from configured origin")
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.apiKey == "" || request.Header.Get("Authorization") != "" {
		return base.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return base.RoundTrip(clone)
}

func safeEmbeddingOrigin(rawURL string, trustPrivateNetwork bool) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("embedding: invalid base_url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("embedding: base_url must use HTTP or HTTPS")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("embedding: base_url must include a host")
	}
	if parsed.Scheme == "http" && !safePlaintextHost(parsed.Hostname(), trustPrivateNetwork) {
		return "", fmt.Errorf("embedding: plaintext HTTP endpoint requires loopback or trusted private network")
	}
	return canonicalOrigin(parsed)
}

func safePlaintextHost(host string, trustPrivateNetwork bool) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if !trustPrivateNetwork {
		return false
	}
	return ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || isCGNAT(ip)
}

func isCGNAT(ip net.IP) bool {
	ip = ip.To4()
	return ip != nil && ip[0] == 100 && ip[1]&0xc0 == 0x40
}

func canonicalOrigin(parsed *url.URL) (string, error) {
	if parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("embedding: invalid request origin")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "https" && port == "443") || (parsed.Scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return strings.ToLower(parsed.Scheme) + "://" + host, nil
}

func canonicalEmbeddingEndpoint(parsed *url.URL) (string, error) {
	origin, err := canonicalOrigin(parsed)
	if err != nil {
		return "", err
	}
	endpointPath := strings.TrimRight(parsed.EscapedPath(), "/")
	return origin + endpointPath, nil
}

// Generation identifies every configured input that affects the vector space.
func (c *Client) Generation() vector.Generation {
	params := map[string]string{
		"endpoint":        c.endpoint,
		"input_type_mode": c.inputTypeMode,
		"recipe":          strconv.Itoa(c.recipeVersion),
	}
	if c.salt != "" {
		params["salt"] = c.salt
	}
	return vector.Generation{Model: c.model, Dimensions: c.dims, Params: params}
}

// BatchSize returns the maximum number of inputs sent in one provider request.
func (c *Client) BatchSize() int { return c.batchSize }

// EncodeDocuments adapts the document embedding path to vector.EncodeFunc.
func (c *Client) EncodeDocuments() vector.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		return c.Embed(ctx, InputDocument, texts)
	}
}

type embedRequest struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	InputType string   `json:"input_type,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Embedding json.RawMessage `json:"embedding"`
		Index     *int            `json:"index"`
	} `json:"data"`
}

// APIError is a sanitized non-2xx response from the embedding endpoint.
type APIError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("embedding endpoint returned %d: %s", e.StatusCode, providerErrorMessage(e.StatusCode))
}

// Definitive reports whether retrying cannot help without operator action or
// different input.
func (e *APIError) Definitive() bool {
	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func providerErrorMessage(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "embedding request rejected"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "embedding authentication rejected"
	case http.StatusNotFound:
		return "embedding endpoint or model not found"
	case http.StatusTooManyRequests:
		return "embedding rate limit exceeded"
	default:
		if status >= 500 {
			return "embedding provider unavailable"
		}
		return "embedding provider rejected request"
	}
}

// Embed returns one L2-normalized vector per input, preserving input order.
func (c *Client) Embed(ctx context.Context, kind InputKind, texts []string) ([][]float32, error) {
	if kind != InputDocument && kind != InputQuery {
		return nil, fmt.Errorf("embedding input kind must be %q or %q", InputDocument, InputQuery)
	}
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := min(start+c.batchSize, len(texts))
		vectors, err := c.embedBatch(ctx, kind, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, kind InputKind, texts []string) ([][]float32, error) {
	inputType := ""
	if c.inputTypeMode == "retrieval" {
		inputType = string(kind)
	}
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts, InputType: inputType})
	if err != nil {
		return nil, fmt.Errorf("embedding: encode request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: create request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, sanitizeRequestError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: response.StatusCode,
			RetryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
		}
	}

	var decoded embedResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("embedding response is invalid")
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("embedding response contained %d vectors for %d inputs", len(decoded.Data), len(texts))
	}
	vectors := make([][]float32, len(decoded.Data))
	seen := make([]bool, len(decoded.Data))
	for itemPosition, item := range decoded.Data {
		if item.Index == nil {
			return nil, fmt.Errorf("embedding response item %d is missing index", itemPosition)
		}
		index := *item.Index
		if index < 0 || index >= len(decoded.Data) {
			return nil, fmt.Errorf(
				"embedding response item %d has index %d; expected 0..%d",
				itemPosition, index, len(decoded.Data)-1,
			)
		}
		if seen[index] {
			return nil, fmt.Errorf("embedding response contains duplicate index %d", index)
		}
		vector, err := c.decodeVector(index, item.Embedding)
		if err != nil {
			return nil, err
		}
		vectors[index] = vector
		seen[index] = true
	}
	return vectors, nil
}

func (c *Client) decodeVector(index int, raw json.RawMessage) ([]float32, error) {
	var components []*float64
	if len(raw) == 0 || json.Unmarshal(raw, &components) != nil {
		return nil, fmt.Errorf("embedding vector %d is invalid", index)
	}
	if len(components) != c.dims {
		return nil, fmt.Errorf("embedding vector %d has %d dimensions; expected %d", index, len(components), c.dims)
	}
	result := make([]float32, len(components))
	var sumSquares float64
	for componentIndex, component := range components {
		if component == nil {
			return nil, fmt.Errorf("embedding vector %d component %d is null", index, componentIndex)
		}
		value := float32(*component)
		if math.IsNaN(*component) || math.IsInf(*component, 0) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("embedding vector %d component %d is not finite", index, componentIndex)
		}
		result[componentIndex] = value
		sumSquares += float64(value) * float64(value)
	}
	if sumSquares == 0 {
		return nil, fmt.Errorf("embedding vector %d has zero norm", index)
	}
	norm := math.Sqrt(sumSquares)
	for i := range result {
		result[i] = float32(float64(result[i]) / norm)
	}
	return result, nil
}

func sanitizeRequestError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("embedding: request failed: %w", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("embedding: request failed: %w", context.DeadlineExceeded)
	default:
		return errors.New("embedding: request failed")
	}
}

func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0
	}
	if delay := time.Until(when); delay > 0 {
		return delay
	}
	return 0
}
