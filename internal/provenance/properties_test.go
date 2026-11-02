package provenance

import (
	"encoding/hex"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/roborev/internal/config"
)

func TestResolvedPrefixProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sha := hex.EncodeToString(hegel.Draw(ht, hegel.Binary(20, 20)))
		n := hegel.Draw(ht, hegel.Integers(7, 40))
		link := sampleLink()
		link.CommitSHA = sha
		got := Resolve([]Input{{SHA: sha[:n]}}, LookupResult{Commits: map[string][]Link{sha[:n]: {link}}})
		require.Len(ht, got, 1)
		require.Len(ht, got[0].Sessions, 1)
		assert.Equal(ht, sha, got[0].Sessions[0].CommitSHA)
		assert.False(ht, got[0].Sessions[0].Ambiguous)
	}, hegel.WithTestCases(2000))
}

func TestUnresolvedEvidenceAlwaysAmbiguousProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sha := hex.EncodeToString(hegel.Draw(ht, hegel.Binary(20, 20)))
		n := hegel.Draw(ht, hegel.Integers(7, 40))
		link := sampleLink()
		link.CommitSHA = sha[:n]
		link.Resolved = false
		for _, request := range []string{sha, sha[:n]} {
			got := Resolve([]Input{{SHA: request}}, LookupResult{Commits: map[string][]Link{request: {link}}})
			require.Len(ht, got, 1)
			require.Len(ht, got[0].Sessions, 1)
			assert.True(ht, got[0].Sessions[0].Ambiguous)
		}
	}, hegel.WithTestCases(2000))
}

func TestEvidenceCoordinatesAndTurnsSurviveProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		prompt := hegel.Draw(ht, hegel.Text())
		first := sampleLink()
		second := sampleLink()
		second.MessageOrdinal = new(n)
		second.CallIndex = new(0)
		second.Turn = &Turn{Ordinal: n, Prompt: prompt, Why: "commit-call"}
		got := Resolve([]Input{{SHA: testSHA}}, LookupResult{Commits: map[string][]Link{testSHA: {first, second, first}}})
		require.Len(ht, got, 1)
		require.Len(ht, got[0].Sessions, 2)
		assert.Equal(ht, second.MessageOrdinal, got[0].Sessions[1].MessageOrdinal)
		assert.Equal(ht, second.CallIndex, got[0].Sessions[1].CallIndex)
		assert.Equal(ht, second.Turn, got[0].Sessions[1].Turn)
	}, hegel.WithTestCases(2000))
}

func TestLookupOversizeSentinel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"HTTP422", 422, `{"error":{"message":"commit lookup exceeds result limits"}}`},
		{"bytes", 200, strings.Repeat(" ", maxResponseBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.AgentsView.URL = srv.URL
			_, err := new(Client).Lookup(t.Context(), "", []string{testSHA}, cfg)
			require.ErrorIs(t, err, ErrLookupTooLarge)
		})
	}
}
