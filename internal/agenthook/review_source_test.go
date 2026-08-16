package agenthook

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestHTTPReviewSourceRejectsPublicHTTPAddress(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"tracked":true,"repo":{"root_path":"/repo","identity":"repo-a","name":"repo-a"}}`,
			)),
			Header: make(http.Header),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })

	_, known := NewHTTPReviewSource("http://192.0.2.1:7373").ResolveTrackedRepo(
		context.Background(), "/repo", "main",
	)

	assert.False(t, known)
}
