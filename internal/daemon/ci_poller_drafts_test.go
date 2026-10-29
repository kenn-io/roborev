package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCIPollerDraftAPITransport(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				pr := fmt.Sprintf(`{"number":9,"state":"open","draft":%t,"head":{"sha":"head-a","ref":"feature"},"base":{"ref":"main"}}`, draft)
				if r.URL.Path == "/api/v3/repos/acme/api/pulls" {
					pr = "[" + pr + "]"
				}
				fmt.Fprint(w, pr)
			}))
			t.Cleanup(api.Close)
			h := newCIPollerHarness(t, "https://github.com/acme/api.git")
			h.Poller.githubAPIURL = api.URL + "/api/v3"
			prs, err := h.Poller.listOpenPRs(t.Context(), "acme/api")
			require.NoError(t, err)
			require.Len(t, prs, 1)
			assert.Equal(t, draft, prs[0].Draft)
			target, err := h.Poller.panelPostTarget(t.Context(), "acme/api", 9)
			require.NoError(t, err)
			assert.Equal(t, draft, target.Draft)
		})
	}
}
