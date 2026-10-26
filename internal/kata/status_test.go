package kata

import (
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
)

func kataConfigFor(rawURL string) config.KataIntegrationConfig {
	return config.KataIntegrationConfig{
		Enabled: true, Endpoint: rawURL, APIKey: testAPIKey, DefaultProject: "reviews", Teammate: "roborev",
	}
}

func TestEvaluateDisabledMakesNoRequest(t *testing.T) {
	f := katafake.New(t)
	cfg := kataConfigFor(f.URL())
	cfg.Enabled = false
	status := EvaluateWith(t.Context(), t.TempDir(), cfg, "")
	assert.Equal(t, StateDisabled, status.State)
	assert.Empty(t, f.AllRequests())
}

func TestEvaluateProjectSelection(t *testing.T) {
	for _, tc := range []struct {
		name, binding, dflt, override, wantProject string
		want                                       State
	}{
		{"named project", "reviews", "reviews", "other", "other", StateReady},
		{"missing named project", "reviews", "reviews", "ghost", "ghost", StateProjectNotFound},
		{"no selection", "", "", "", "", StateProjectNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := katafake.New(t)
			cfg := kataConfigFor(f.URL())
			cfg.DefaultProject = tc.dflt
			status := EvaluateWith(t.Context(), namedWorkspace(t, tc.binding), cfg, tc.override)
			assert := assert.New(t)
			assert.Equal(tc.want, status.State, status.Message)
			assert.Equal(tc.wantProject, status.Project)
			assert.Equal("v0.19.0", status.ServerVersion)
			assert.Equal("0.28.0", status.APISchemaVersion)
			if tc.wantProject == "" {
				assert.Contains(status.Message, "default_project")
			}
			encoded, err := json.Marshal(status)
			require.NoError(t, err)
			assert.NotContains(string(encoded), testAPIKey)
		})
	}
}

func TestEvaluateWithClosesDiscardedClientConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"version":"v0.19.0","api_schema_version":"0.28.0"}`))
		case "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[{"id":7,"name":"reviews","uid":"P0000000000000000000000007","active":true}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	defer srv.Close()

	status := EvaluateWith(t.Context(), namedWorkspace(t, "reviews"), kataConfigFor(srv.URL), "reviews")
	require.Equal(t, StateReady, status.State)
	// This waits for the external HTTP socket to close in the server goroutine.
	require.Eventually(t, func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "EvaluateWith must close its discarded client's idle connection")
}

func TestStatusForError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want State
	}{
		{ErrPlatformSecurityLimit, StateIncompatible},
		{ErrInsecureEndpoint, StateIncompatible},
		{ErrIncompatible, StateIncompatible},
		{ErrInvalidResponse, StateIncompatible},
		{ErrRedirect, StateIncompatible},
		{ErrRequestRejected, StateIncompatible},
		{ErrNotFound, StateIncompatible},
		{ErrAuthenticationRequired, StateAuthenticationRequired},
		{ErrProjectNotFound, StateProjectNotFound},
		{ErrNoBinding, StateProjectNotFound},
		{ErrUnreachable, StateUnreachable},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			status := statusForError(fmt.Errorf("context: %w", tc.err), "reviews")
			assert.Equal(t, tc.want, status.State)
			assert.Equal(t, "reviews", status.Project)
			assert.NotEmpty(t, status.Message)
		})
	}
}

// Construction failures must reach the same states without exposing the
// credential, even before a client exists to evaluate the connection.
func TestEvaluateConstructionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, keyFile string
		want                    State
	}{
		{"unsupported endpoint", "ftp://" + testAPIKey, "", StateIncompatible},
		{"unreadable credential", "https://kata.example.com", filepath.Join(t.TempDir(), "missing"), StateAuthenticationRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := kataConfigFor(tc.endpoint)
			if tc.keyFile != "" {
				cfg.APIKey, cfg.APIKeyFile = "", tc.keyFile
			}
			status := EvaluateWith(t.Context(), t.TempDir(), cfg, "")
			assert.Equal(t, tc.want, status.State)
			encoded, err := json.Marshal(status)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), testAPIKey)
		})
	}
}

// Typed replies are stored from API schema 0.28.0, and only a ready
// connection reports support for them.
func TestStatusSupportsRepliesFromSchema0_28(t *testing.T) {
	for _, tc := range []struct {
		state  State
		schema string
		want   bool
	}{
		{StateReady, "0.21.0", false},
		{StateReady, "0.27.9", false},
		{StateReady, "0.28.0", true},
		{StateReady, "v0.31.2", true},
		{StateReady, "1.0.0", true},
		{StateIncompatible, "0.28.0", false},
		{StateProjectNotFound, "0.28.0", false},
	} {
		assert.Equal(t, tc.want, Status{State: tc.state, APISchemaVersion: tc.schema}.SupportsReplies(), "%s %q", tc.state, tc.schema)
	}
}
