package github

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	googlegithub "github.com/google/go-github/v91/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type retryTransport func(*http.Request) (*http.Response, error)

func (f retryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type interruptedBody struct{ closed bool }

func (b *interruptedBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *interruptedBody) Close() error             { b.closed = true; return nil }

func TestGitHubReadRetries(t *testing.T) {
	for _, failure := range []string{"status", "body", "network", "timeout", "body timeout", "rate limit", "retry after date"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts int
				var interrupted interruptedBody
				var diagnostics bytes.Buffer
				old := log.Writer()
				log.SetOutput(&diagnostics)
				defer log.SetOutput(old)
				started := time.Now()
				client, err := NewClient("synthetic-token", WithHTTPClient(&http.Client{Timeout: 30 * time.Second, Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
					attempts++
					assert.Equal(t, http.MethodGet, r.Method)
					assert.Equal(t, "/repos/acme/api/pulls", r.URL.Path)
					assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
					resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`[{"number":7}]`)), Request: r}
					if attempts == 1 {
						switch failure {
						case "status":
							resp.StatusCode = 503
						case "body":
							resp.Body = struct {
								io.Reader
								io.Closer
							}{io.MultiReader(strings.NewReader(`[{"number":9},`), &interrupted), &interrupted}
						case "network":
							return nil, io.ErrUnexpectedEOF
						case "timeout":
							<-r.Context().Done()
							return nil, r.Context().Err()
						case "body timeout":
							resp.Body = &timeoutBody{ctx: r.Context()}
						case "rate limit":
							resp.StatusCode = 429
							resp.Header.Set("Retry-After", "3")
						case "retry after date":
							resp.StatusCode = 503
							resp.Header.Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
						}
					}
					return resp, nil
				})}))
				require.NoError(t, err)
				prs, err := client.ListOpenPullRequests(t.Context(), "acme/api")
				require.NoError(t, err)
				require.Len(t, prs, 1)
				assert.Equal(t, 7, prs[0].Number)
				assert.Equal(t, 2, attempts)
				assert.Contains(t, diagnostics.String(), "list pull requests")
				assert.NotContains(t, diagnostics.String(), "synthetic-token")
				if failure == "body" {
					assert.True(t, interrupted.closed)
				}
				if failure == "timeout" || failure == "body timeout" {
					assert.Greater(t, time.Since(started), 30*time.Second)
				}
				if failure == "rate limit" || failure == "retry after date" {
					assert.Equal(t, 3*time.Second, time.Since(started))
				}
			})
		})
	}
}

func TestGitHubReadFailureBudget(t *testing.T) {
	for _, tc := range []struct{ status, wantAttempts int }{{503, 4}, {408, 4}, {429, 4}, {401, 1}, {403, 1}, {404, 1}, {422, 1}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				client, err := NewClient("", WithHTTPClient(&http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
					attempts++
					return &http.Response{StatusCode: tc.status, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"sensitive provider text"}`))}, nil
				})}))
				require.NoError(t, err)
				_, err = client.ListOpenPullRequests(t.Context(), "acme/api")
				require.Error(t, err)
				assert.Equal(t, tc.wantAttempts, attempts)
				var responseError *googlegithub.ErrorResponse
				require.ErrorAs(t, err, &responseError)
				assert.Equal(t, tc.status, responseError.Response.StatusCode)
				assert.NotContains(t, err.Error(), "sensitive provider text")
			})
		})
	}
}

func TestGitHubReadCancellation(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "backoff", true: "in flight"}[inFlight], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				attempts := 0
				client, err := NewClient("", WithHTTPClient(&http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
					attempts++
					if inFlight {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 429, Request: r, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				})}))
				require.NoError(t, err)
				_, err = client.ListOpenPullRequests(ctx, "acme/api")
				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.Equal(t, 1, attempts)
			})
		})
	}
}

func TestGitHubWritesAreNotRetried(t *testing.T) {
	for _, networkFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "status", true: "network"}[networkFailure], func(t *testing.T) {
			attempts := 0
			client, err := NewClient("", WithHTTPClient(&http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
				attempts++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/repos/acme/api/statuses/head", r.URL.Path)
				if networkFailure {
					return nil, errors.New("connection interrupted")
				}
				return &http.Response{StatusCode: 503, Request: r, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}))
			require.NoError(t, err)
			err = client.SetCommitStatus(t.Context(), "acme/api", "head", "pending", "queued")
			require.Error(t, err)
			assert.Equal(t, 1, attempts)
		})
	}
}

func TestGitHubLongRetryAfterSurfacesRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		started := time.Now()
		client, err := NewClient("", WithHTTPClient(&http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{StatusCode: 403, Request: r, Header: http.Header{"Retry-After": []string{"121"}, "X-Ratelimit-Remaining": []string{"0"}}, Body: io.NopCloser(strings.NewReader(`{"message":"sensitive rate limit text"}`))}, nil
		})}))
		require.NoError(t, err)
		_, err = client.ListOpenPullRequests(t.Context(), "acme/api")
		var rate *googlegithub.RateLimitError
		require.ErrorAs(t, err, &rate)
		assert.Equal(t, 1, attempts)
		assert.Zero(t, time.Since(started))
		assert.Equal(t, "list pull requests: rate limit (HTTP 403)", err.Error())
	})
}

type timeoutBody struct{ ctx context.Context }

func (b *timeoutBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *timeoutBody) Close() error             { return nil }

func TestGitHubRetryDoesNotDuplicateEarlierPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := 0, 0
		client, err := NewClient("", WithHTTPClient(&http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
			resp := &http.Response{StatusCode: 200, Request: r, Header: make(http.Header)}
			if r.URL.Query().Get("page") == "" {
				first++
				resp.Header.Set("Link", `<https://api.github.com/repos/acme/api/pulls?page=2>; rel="next"`)
				resp.Body = io.NopCloser(strings.NewReader(`[{"number":1}]`))
			} else {
				assert.Equal(t, "2", r.URL.Query().Get("page"))
				second++
				if second == 1 {
					resp.Body = &interruptedBody{}
				} else {
					resp.Body = io.NopCloser(strings.NewReader(`[{"number":2}]`))
				}
			}
			return resp, nil
		})}))
		require.NoError(t, err)
		prs, err := client.ListOpenPullRequests(t.Context(), "acme/api")
		require.NoError(t, err)
		require.Len(t, prs, 2)
		assert.Equal(t, []int{1, 2}, []int{prs[0].Number, prs[1].Number})
		assert.Equal(t, 1, first)
		assert.Equal(t, 2, second)
	})
}
