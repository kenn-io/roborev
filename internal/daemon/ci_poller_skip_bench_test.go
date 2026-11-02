package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// useCISkipAPI shares the health test's in-memory GitHub transport setup.
func useCISkipAPI(t testing.TB, p *CIPoller, handler http.Handler) {
	t.Helper()
	api := httptest.NewTestServer(t, handler)
	transport := http.DefaultTransport
	http.DefaultTransport = api.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })
	p.githubAPIURL = api.URL + "/api/v3"
	p.setCommitStatusFn = nil
}

func BenchmarkCIPollSkippedPRs(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.Setenv("GH_TOKEN", "test-token")
			h := newCIPollerHarness(b, "https://github.com/acme/api.git")
			h.Cfg.CI.SkipDrafts = true
			prs := make([]ghPR, n)
			for i := range prs {
				prs[i] = ghPR{Number: i + 1, HeadRefOid: fmt.Sprintf("head-%d", i), Draft: true}
			}
			h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) { return prs, nil }
			var reads atomic.Int64
			useCISkipAPI(b, h.Poller, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
					reads.Add(1)
					fmt.Fprint(w, `{"statuses":[{"context":"roborev","state":"success","description":"Review skipped: draft"}]}`)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/check-runs"):
					reads.Add(1)
					fmt.Fprint(w, `{"check_runs":[{"conclusion":"skipped","output":{"summary":"Review skipped: draft"}}]}`)
				default:
					b.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			// Exercise the real Checks API without provisioning GitHub App credentials.
			h.Poller.setSkippedCheckFn = func(repo, sha, summary string) error {
				client, err := h.Poller.githubClientForRepo(repo)
				if err != nil {
					return err
				}
				return client.EnsureSkippedCheckRun(b.Context(), repo, sha, summary)
			}
			require.NoError(b, h.Poller.pollRepo(b.Context(), "acme/api", h.Cfg))
			require.Equal(b, int64(2*n), reads.Swap(0), "warm poll reads both GitHub channels")
			for b.Loop() {
				require.NoError(b, h.Poller.pollRepo(b.Context(), "acme/api", h.Cfg))
			}
			b.ReportMetric(float64(reads.Load())/float64(b.N), "GETs/poll")
		})
	}
}
