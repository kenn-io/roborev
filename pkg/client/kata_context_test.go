package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestClassifyKataContextError(t *testing.T) {
	for _, tc := range []struct {
		status int64
		want   error
	}{
		{http.StatusUnauthorized, ErrKataAuthentication},
		{http.StatusForbidden, ErrKataAccess},
		{http.StatusRequestTimeout, context.Canceled},
		{http.StatusGatewayTimeout, context.DeadlineExceeded},
	} {
		err := ClassifyKataContextError(generated.ErrorModel{Status: &tc.status})
		require.ErrorIs(t, err, tc.want)
	}
}

func TestKataContextErrorThroughGeneratedClient(t *testing.T) {
	for _, tc := range []struct { status int; want error }{
		{http.StatusUnauthorized, ErrKataAuthentication},
		{http.StatusForbidden, ErrKataAccess},
		{http.StatusRequestTimeout, context.Canceled},
		{http.StatusGatewayTimeout, context.DeadlineExceeded},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
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
