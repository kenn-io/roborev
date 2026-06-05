package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.kenn.io/roborev/internal/daemon"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// errNotFound is returned for 404 daemon API responses.
// Callers can detect it with errors.Is(err, errNotFound).
var errNotFound = errors.New("not found")

// readErrorBody reads a JSON error response body and returns the "error" field,
// falling back to the raw body text or HTTP status.
func readErrorBody(body io.Reader, status string) string {
	data, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil || len(data) == 0 {
		return status
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &errResp) == nil && errResp.Error != "" {
		return errResp.Error
	}
	if s := strings.TrimSpace(string(data)); s != "" {
		return s
	}
	return status
}

func newDaemonAPI(
	ep daemon.DaemonEndpoint,
	httpClient *http.Client,
) *roborevclient.Client {
	client, err := roborevclient.NewWithHTTPClient(
		ep.BaseURL(),
		httpClient,
	)
	if err != nil {
		panic(fmt.Sprintf("create daemon API client for %q: %v", ep.BaseURL(), err))
	}
	return client
}

func readErrorBytes(body []byte, status string) string {
	return readErrorBody(bytes.NewReader(body), status)
}

func apiStatus(statusCode int) string {
	return fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
}

func apiStatusError(statusCode int, status string, body []byte) error {
	if statusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", errNotFound, readErrorBytes(body, status))
	}
	return fmt.Errorf("%s", readErrorBytes(body, status))
}

func decodeAPIBody(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (m model) apiContext() context.Context {
	return context.Background()
}
