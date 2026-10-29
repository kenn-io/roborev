package tokens

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentsViewRequiresRawSuffixAcknowledgement(t *testing.T) {
	for _, ack := range []string{"", `,"raw_suffix":false`} {
		t.Run(ack, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch calls {
				case 1:
					w.WriteHeader(404)
				case 2:
					fmt.Fprintf(w, `{"ids":["codex:unrelated"]%s}`, ack)
				default:
					fmt.Fprint(w, `{"has_token_data":false,"has_cost":true,"cost_usd":99}`)
				}
			}))
			defer srv.Close()
			usage, err := FetchForSessionWithConfig(t.Context(), "raw-id", FetchConfig{AgentsViewURL: srv.URL})
			require.ErrorContains(t, err, "does not acknowledge raw session ID lookup")
			assert.Nil(t, usage)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestAgentsViewNestedAmbiguity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		fmt.Fprint(w, `{"error":{"code":"ambiguous_session_id","message":"ambiguous session ID","candidates":["codex:a","host~codex:a"]}}`)
	}))
	defer srv.Close()
	_, err := FetchForSessionWithConfig(t.Context(), "a", FetchConfig{AgentsViewURL: srv.URL})
	require.ErrorContains(t, err, "ambiguous")
	require.ErrorContains(t, err, "codex:a")
	require.ErrorContains(t, err, "host~codex:a")
}

func TestAgentsViewProbeIdentity(t *testing.T) {
	for _, body := range []string{`<html>private-server-body</html>`, `{}`, `{"ok":false,"service":"agentsview"}`, `{"ok":true,"service":"other"}`, `{"ok":true,"service":"agentsview"}`} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			err := (FetchConfig{AgentsViewURL: srv.URL}).ProbeAgentsView(t.Context())
			if body == `{"ok":true,"service":"agentsview"}` {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "private-server-body")
			}
		})
	}
}

func TestAgentsViewProbePreservesTransportError(t *testing.T) {
	cause := errors.New("synthetic TLS handshake failure")
	cfg := FetchConfig{
		AgentsViewURL: "https://usage.example",
		Client: &http.Client{Transport: agentsViewRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, cause
		})},
	}
	err := cfg.ProbeAgentsView(t.Context())
	require.ErrorIs(t, err, cause)
	assert.ErrorContains(t, err, "synthetic TLS handshake failure")
}
