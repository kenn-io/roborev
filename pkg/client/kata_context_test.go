package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/client/generated"
)

func TestKataContextErrorThroughGeneratedClient(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrKataAuthentication},
		{http.StatusForbidden, ErrKataAccess},
		{http.StatusRequestTimeout, context.Canceled},
		{http.StatusGatewayTimeout, context.DeadlineExceeded},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			status := int64(tc.status)
			require.ErrorIs(t, ClassifyKataContextError(generated.ErrorModel{Status: &status}), tc.want)
			bodyOnly := runtime.NewClientAPIError(generated.ErrorModel{Status: &status})
			require.ErrorIs(t, ClassifyKataContextError(bodyOnly), tc.want)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"status":%d,"detail":"read failed"}`, tc.status)
			}))
			t.Cleanup(server.Close)
			api, err := NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			_, err = api.KataPublication(t.Context(), &generated.KataPublicationRequestOptions{Query: &generated.KataPublicationQuery{Issue: new("issue")}})
			require.ErrorIs(t, ClassifyKataContextError(err), tc.want)
		})
	}
}

func TestKataContextErrorUsesHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    error
		notWant error
	}{
		{"middleware authentication", http.StatusUnauthorized, `{"error":"daemon authentication required"}`, ErrKataAuthentication, ErrKataAccess},
		{"HTTP status overrides body", http.StatusUnauthorized, `{"status":403}`, ErrKataAuthentication, ErrKataAccess},
		{"nonfatal HTTP status overrides body", http.StatusServiceUnavailable, `{"status":401}`, nil, ErrKataAuthentication},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)
			api, err := NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			_, err = api.KataPublication(t.Context(), &generated.KataPublicationRequestOptions{Query: &generated.KataPublicationQuery{Issue: new("issue")}})
			require.Error(t, err)
			wrapped := fmt.Errorf("read publication: %w", err)
			classified := ClassifyKataContextError(wrapped)
			if tc.want != nil {
				require.ErrorIs(t, classified, tc.want)
			}
			require.NotErrorIs(t, classified, tc.notWant)
			require.ErrorIs(t, classified, err)
		})
	}
}
