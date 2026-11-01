package provenance

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

const (
	testSHA = "0123456789abcdef0123456789abcdef01234567"
	testID  = "codex:01234567-89ab-4cde-8fab-0123456789ab"
)

func sampleLink() Link {
	return Link{SessionID: testID, CommitSHA: testSHA, Resolved: true, Source: "transcript", Agent: "codex", Session: &Identity{ID: testID}, Turn: &Turn{Ordinal: 0, Prompt: "Fix parser", Why: "commit-call"}, WebURL: "/sessions/example"}
}

func TestClientHTTPContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/base/api/v1/commits/sessions", r.URL.Path)
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
		var request struct {
			SHAs []string `json:"shas"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.Equal(t, []string{testSHA}, request.SHAs)
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"commits": map[string][]Link{testSHA: {sampleLink()}}}))
	}))
	defer srv.Close()
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "synthetic-token")
	cfg := config.DefaultConfig()
	cfg.AgentsView.URL = srv.URL + "/base"
	result, err := new(Client).Lookup(t.Context(), t.TempDir(), []string{testSHA}, cfg)
	require.NoError(t, err)
	got := Resolve([]Input{{SHA: testSHA}}, result)
	require.Len(t, got, 1)
	assert.Equal(t, "http", got[0].Coverage)
	require.Len(t, got[0].Sessions, 1)
	assert.Equal(t, srv.URL+"/sessions/example", got[0].Sessions[0].WebURL)
	assert.Equal(t, "Fix parser", got[0].Sessions[0].Turn.Prompt)
}

func TestClientUnsupportedAndTransient(t *testing.T) {
	for _, status := range []int{404, 401, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(status) }))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			c := new(Client)
			for range 2 {
				_, err := c.Lookup(t.Context(), "", []string{testSHA}, cfg)
				require.Error(t, err)
			}
			want := 2
			if status == 404 {
				want = 1
			}
			assert.Equal(t, want, calls)
		})
	}
}

func TestClientCLIReadOnlyContract(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentsview")
	script := `#!/bin/sh
[ "$1" = why ] && [ "$2" = --commit ] && [ "$3" = ` + testSHA + ` ] && [ "$4" = --format ] && [ "$5" = json ] && [ "$6" = --no-sync ] || exit 9
printf '%s' '{"refs":[{"key":"session:` + testID + `","how":"direct","card":{"session_id":"` + testID + `","commit_sha":"` + testSHA + `","source":"transcript","resolved":true,"agent":"codex","session":{"id":"` + testID + `"},"turn":null}}]}'
`
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	cfg := config.DefaultConfig()
	result, err := (&Client{Command: bin}).Lookup(t.Context(), dir, []string{testSHA}, cfg)
	require.NoError(t, err)
	got := Resolve([]Input{{SHA: testSHA}}, result)
	require.Len(t, got, 1)
	assert.Equal(t, "why-selected", got[0].Coverage)
	assert.Equal(t, testID, got[0].Sessions[0].SessionID)
}

func TestResolvePreservesEvidenceCertainty(t *testing.T) {
	exact := sampleLink()
	prefix := sampleLink()
	prefix.CommitSHA = testSHA[:7]
	prefix.Resolved = false
	prefix.Ambiguous = true
	prefix.Turn = &Turn{Prompt: "uncertain", Why: "before"}
	input := []Input{{SHA: testSHA, Message: "Agent-Session: " + testID}}
	got := Resolve(input, LookupResult{Commits: map[string][]Link{testSHA: {exact, prefix, exact}}, Coverage: map[string]string{testSHA: "http"}})
	require.Len(t, got, 1)
	require.Len(t, got[0].Sessions, 3)
	assert.Equal(t, "unknown", got[0].Sessions[0].Availability)
	assert.False(t, got[0].Sessions[1].Ambiguous)
	assert.True(t, got[0].Sessions[2].Ambiguous)
	assert.Equal(t, "uncertain", got[0].Sessions[2].Turn.Prompt)
}

func TestClientRejectsOversizedAndMalformedHTTP(t *testing.T) {
	for _, body := range []string{`{}`, `{"commits":{}}`, strings.Repeat(" ", 8*1024*1024+1)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			_, err := new(Client).Lookup(context.Background(), "", []string{testSHA}, cfg)
			require.Error(t, err)
		})
	}
}

func TestClientHTTPPartialAndProviderChange(t *testing.T) {
	var shas []string
	for i := range 201 {
		shas = append(shas, fmt.Sprintf("%040x", i))
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			SHAs []string `json:"shas"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		assert.LessOrEqual(t, len(request.SHAs), 200)
		if calls == 2 {
			w.WriteHeader(404)
			return
		}
		commits := map[string][]Link{}
		for _, sha := range request.SHAs {
			commits[sha] = []Link{}
		}
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"commits": commits}))
	}))
	defer srv.Close()
	cfg := config.DefaultConfig()
	cfg.AgentsView.URL = srv.URL
	c := new(Client)
	result, err := c.Lookup(t.Context(), "", shas, cfg)
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Len(t, result.Coverage, 200)
	assert.Equal(t, "http", result.Coverage[shas[0]])
	inputs := make([]Input, len(shas))
	for i, sha := range shas {
		inputs[i] = Input{SHA: sha}
	}
	captured := Resolve(inputs, result)
	require.Len(t, captured, 201)
	for _, commit := range captured[:200] {
		assert.Equal(t, "http", commit.Coverage)
		assert.Equal(t, []Session{}, commit.Sessions)
	}
	assert.Equal(t, "trailers", captured[200].Coverage)
	assert.Equal(t, []Session{}, captured[200].Sessions)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"commits":{}}`) }))
	defer other.Close()
	cfg.AgentsView.URL = other.URL
	_, err = c.Lookup(t.Context(), "", nil, cfg)
	require.NoError(t, err, "another provider must not inherit the unsupported cache")
}

func TestClientNeverFollowsAuthenticatedRedirect(t *testing.T) {
	received := 0
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received++ }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "synthetic-token")
	cfg := config.DefaultConfig()
	cfg.AgentsView.URL = source.URL
	_, err := new(Client).Lookup(t.Context(), "", []string{testSHA}, cfg)
	require.Error(t, err)
	assert.Zero(t, received)
}

func TestResolveGoneAndUnrelated(t *testing.T) {
	gone := sampleLink()
	gone.Session = nil
	gone.Agent = ""
	gone.Turn = nil
	gone.WebURL = ""
	gone.Gone = true
	unrelated := sampleLink()
	unrelated.CommitSHA = strings.Repeat("f", 40)
	result := LookupResult{Commits: map[string][]Link{testSHA: {gone, unrelated}}}
	got := Resolve([]Input{{SHA: testSHA}}, result)
	require.Len(t, got, 1)
	require.Len(t, got[0].Sessions, 1)
	assert.Equal(t, "gone", got[0].Sessions[0].Availability)
}

func TestClientCanceledBeforeLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := new(Client).Lookup(ctx, "", []string{testSHA}, config.DefaultConfig())
	require.ErrorIs(t, err, context.Canceled)
}

func TestClientProtocolBounds(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprint(overflow), func(t *testing.T) {
			payload := `{"commits":{"` + testSHA + `":[]}}`
			size := maxResponseBytes
			if overflow {
				size++
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, payload+strings.Repeat(" ", size-len(payload)))
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			_, err := new(Client).Lookup(t.Context(), "", []string{testSHA}, cfg)
			if overflow {
				require.ErrorIs(t, err, ErrLookupTooLarge)
			} else {
				require.NoError(t, err)
			}
		})
	}
	for _, count := range []int{maxResponseLinks, maxResponseLinks + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			links := make([]Link, count)
			for i := range links {
				links[i] = sampleLink()
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"commits": map[string][]Link{testSHA: links}}))
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			got, err := new(Client).Lookup(t.Context(), "", []string{testSHA}, cfg)
			if count > maxResponseLinks {
				require.ErrorIs(t, err, ErrLookupTooLarge)
				assert.Empty(t, got.Coverage)
			} else {
				require.NoError(t, err)
				assert.Len(t, got.Commits[testSHA], count)
			}
		})
	}
}

func TestClientRejectsMalformedEvidence(t *testing.T) {
	for _, kind := range []string{"source", "marker", "agent", "coordinate", "trailer coordinate", "ordinal", "why", "prompt"} {
		t.Run(kind, func(t *testing.T) {
			link := sampleLink()
			switch kind {
			case "source":
				link.Source = "unknown"
			case "marker":
				link.Session = nil
			case "agent":
				link.Agent = "pi"
			case "coordinate":
				link.CallIndex = new(-1)
			case "trailer coordinate":
				link.Source = "trailer"
				link.CallIndex = new(0)
			case "ordinal":
				link.Turn.Ordinal = -1
			case "why":
				link.Turn.Why = "inferred"
			case "prompt":
				link.Turn.Prompt = strings.Repeat("𐐀", 601)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"commits": map[string][]Link{testSHA: {link}}}))
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			got, err := new(Client).Lookup(t.Context(), "", []string{testSHA}, cfg)
			require.Error(t, err)
			assert.Empty(t, got.Coverage)
		})
	}
	link := sampleLink()
	link.Turn.Prompt = strings.Repeat("𐐀", 600)
	require.NoError(t, validateLink(link), "the provider limit counts Unicode characters, not bytes")
}

func TestResolveSuccessfulEmptyLookup(t *testing.T) {
	for _, coverage := range []string{"http", "why-selected", "trailers"} {
		t.Run(coverage, func(t *testing.T) {
			result := LookupResult{Coverage: map[string]string{testSHA: coverage}}
			captured := Resolve([]Input{{SHA: testSHA}}, result)
			require.Len(t, captured, 1)
			assert.Equal(t, Commit{CommitSHA: testSHA, Coverage: coverage, Sessions: []Session{}}, captured[0])
		})
	}
}
