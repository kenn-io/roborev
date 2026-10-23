package kata

import (
	"encoding/json/v2"
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

func TestEvaluateReady(t *testing.T) {
	f := katafake.New(t)
	status := EvaluateWith(t.Context(), bindWorkspace(t, "other"), kataConfigFor(f.URL()), "")
	assert.Equal(t, StateReady, status.State, status.Message)
	assert.Equal(t, "other", status.Project, "the workspace binding is the project in effect")
	assert.Equal(t, "v0.19.0", status.ServerVersion)
	assert.Equal(t, "0.28.0", status.APISchemaVersion)
}

func TestEvaluateProjectRechecksAReusedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	c := apiClientFromURL(t, srv.URL)
	assert.Equal(t, StateReady, c.EvaluateProject(t.Context(), "reviews").State)

	srv.Close()
	status := c.EvaluateProject(t.Context(), "reviews")
	assert.Equal(t, StateUnreachable, status.State, "a cached successful check must not hide a stopped server")
}

func TestEvaluateUsesTheDefaultProjectForAnUnboundWorkspace(t *testing.T) {
	f := katafake.New(t)
	status := EvaluateWith(t.Context(), bindWorkspace(t, ""), kataConfigFor(f.URL()), "")
	assert.Equal(t, StateReady, status.State)
	assert.Equal(t, "reviews", status.Project)
}

func TestEvaluateWithClosesItsIdleConnection(t *testing.T) {
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

	status := EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), kataConfigFor(srv.URL), "")
	require.Equal(t, StateReady, status.State)
	// This waits for an HTTP connection state transition from the server goroutine.
	require.Eventually(t, func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond, "EvaluateWith must close its discarded client's idle connection")
}

func TestEvaluateReadyFromTheOldestSupportedSchema(t *testing.T) {
	for _, schema := range []string{"0.21.0", "0.26.9", "0.28.0", "0.31.2"} {
		f := katafake.New(t)
		f.APISchema = schema
		status := EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), kataConfigFor(f.URL()), "")
		require.Equal(t, StateReady, status.State, schema)
		assert.Equal(t, schema, status.APISchemaVersion)
	}
}

func TestEvaluateWithANamedProjectIgnoresTheBindingAndTheDefault(t *testing.T) {
	f := katafake.New(t)
	cfg := kataConfigFor(f.URL())
	status := EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), cfg, "other")
	assert.Equal(t, StateReady, status.State, status.Message)
	assert.Equal(t, "other", status.Project)

	status = EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), cfg, "ghost")
	assert.Equal(t, StateProjectNotFound, status.State)
	assert.Equal(t, "ghost", status.Project)
}

func TestEvaluateIncompatible(t *testing.T) {
	t.Run("old API schema", func(t *testing.T) {
		f := katafake.New(t)
		f.APISchema = "0.20.0"
		status := EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), kataConfigFor(f.URL()), "")
		assert.Equal(t, StateIncompatible, status.State)
		assert.Equal(t, "0.20.0", status.APISchemaVersion)
		assert.Empty(t, f.Requests(), "nothing past the health check is sent")
	})
	t.Run("no health endpoint", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusNotFound)
		assert.Equal(t, StateIncompatible, EvaluateWith(t.Context(), t.TempDir(), kataConfigFor(srv.URL), "").State)
	})
	t.Run("not Kata", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
		defer srv.Close()
		assert.Equal(t, StateIncompatible, EvaluateWith(t.Context(), t.TempDir(), kataConfigFor(srv.URL), "").State)
	})
	t.Run("redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
		}))
		defer srv.Close()
		assert.Equal(t, StateIncompatible, EvaluateWith(t.Context(), t.TempDir(), kataConfigFor(srv.URL), "").State)
	})
	t.Run("endpoint that is not allowed", func(t *testing.T) {
		cfg := kataConfigFor("http://kata.example.com")
		status := EvaluateWith(t.Context(), t.TempDir(), cfg, "")
		assert.Equal(t, StateIncompatible, status.State)
		assert.Contains(t, status.Message, "endpoint")
	})
}

func TestEvaluateUnreachable(t *testing.T) {
	t.Run("closed port", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())
		assert.Equal(t, StateUnreachable, EvaluateWith(t.Context(), t.TempDir(), kataConfigFor("http://"+addr), "").State)
	})
	t.Run("server error", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusServiceUnavailable)
		assert.Equal(t, StateUnreachable, EvaluateWith(t.Context(), t.TempDir(), kataConfigFor(srv.URL), "").State)
	})
}

func TestEvaluateAuthenticationRequired(t *testing.T) {
	t.Run("server rejects the credential", func(t *testing.T) {
		f := katafake.New(t)
		f.Token = "another-credential"
		status := EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), kataConfigFor(f.URL()), "")
		assert.Equal(t, StateAuthenticationRequired, status.State)
		assert.NotContains(t, status.Message, testAPIKey)
	})
	t.Run("credential file cannot be read", func(t *testing.T) {
		cfg := kataConfigFor("https://kata.example.com")
		cfg.APIKey, cfg.APIKeyFile = "", filepath.Join(t.TempDir(), "missing")
		assert.Equal(t, StateAuthenticationRequired, EvaluateWith(t.Context(), t.TempDir(), cfg, "").State)
	})
}

func TestEvaluateProjectNotFound(t *testing.T) {
	t.Run("bound project is missing on the server", func(t *testing.T) {
		f := katafake.New(t)
		status := EvaluateWith(t.Context(), bindWorkspace(t, "ghost"), kataConfigFor(f.URL()), "")
		assert.Equal(t, StateProjectNotFound, status.State)
		assert.Equal(t, "ghost", status.Project)
		assert.Equal(t, "0.28.0", status.APISchemaVersion, "the server facts are still reported")
	})
	t.Run("nothing selects a project", func(t *testing.T) {
		f := katafake.New(t)
		cfg := kataConfigFor(f.URL())
		cfg.DefaultProject = ""
		status := EvaluateWith(t.Context(), bindWorkspace(t, ""), cfg, "")
		assert.Equal(t, StateProjectNotFound, status.State)
		assert.Empty(t, status.Project)
		assert.Contains(t, status.Message, "default_project")
	})
}

func TestStatusNeverCarriesTheCredential(t *testing.T) {
	f := katafake.New(t)
	for _, status := range []Status{
		EvaluateWith(t.Context(), bindWorkspace(t, "reviews"), kataConfigFor(f.URL()), ""),
		EvaluateWith(t.Context(), bindWorkspace(t, "ghost"), kataConfigFor(f.URL()), ""),
		EvaluateWith(t.Context(), t.TempDir(), kataConfigFor("ftp://"+testAPIKey), ""),
	} {
		encoded, err := json.Marshal(status)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), testAPIKey)
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
