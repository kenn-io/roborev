package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

// receiptServer answers every report with status and counts the events it sees.
type receiptServer struct {
	*httptest.Server
	mu     sync.Mutex
	events []string
	status atomic.Value // string, or "500" for a failure
}

func newReceiptServer(t *testing.T, status string) *receiptServer {
	t.Helper()
	s := &receiptServer{}
	s.status.Store(status)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Event string `json:"event"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.events = append(s.events, req.Event)
		s.mu.Unlock()
		reply := s.status.Load().(string)
		switch reply {
		case "500":
			http.Error(w, "down", http.StatusInternalServerError)
			return
		case "empty": // an older daemon's 202 carries no receipt
			w.WriteHeader(http.StatusAccepted)
			return
		case "401":
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"` + reply + `"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *receiptServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func probe(g *ClientGate, s *receiptServer, scope, surface string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g.Probe(ctx, s.Client(), scope, s.URL, surface)
}

func post(g *ClientGate, s *receiptServer, scope, event, surface string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	props := map[string]any{PropertySurface: surface}
	_, _ = g.Post(ctx, s.Client(), scope, s.URL, event, props)
}

func TestClientGateSharesOneProbeAcrossInstances(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "queued")
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { probe(&ClientGate{}, s, "daemon", SurfaceCLI) })
	}
	wg.Wait()
	assert.Equal(t, []string{EventAppOpened}, s.seen())
}

func TestClientGateDisabledStopsEveryEventForThatDaemonOnly(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "disabled")
	probe(&ClientGate{}, s, "off", SurfaceCLI)
	require.Equal(t, []string{EventAppOpened}, s.seen())

	fresh := &ClientGate{}
	probe(fresh, s, "off", SurfaceCLI)
	post(fresh, s, "off", EventReviewViewed, SurfaceCLI)
	post(fresh, s, "off", EventAgentActive, "")
	assert.False(t, fresh.Ready("off"))
	assert.Equal(t, []string{EventAppOpened}, s.seen())

	s.status.Store("queued")
	assert.True(t, fresh.Ready("other"))
	probe(fresh, s, "other", SurfaceCLI)
	post(fresh, s, "other", EventReviewViewed, SurfaceCLI)
	assert.Equal(t, []string{EventAppOpened, EventAppOpened, EventReviewViewed}, s.seen())
}

func TestClientGateDisabledReplyToAnyRouteIsSaved(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "disabled")
	post(&ClientGate{}, s, "daemon", EventAgentActive, "")

	fresh := &ClientGate{}
	post(fresh, s, "daemon", EventAgentActive, "")
	assert.Equal(t, []string{EventAgentActive}, s.seen())
}

func TestClientGateProbeBackoffPersistsStopsAtSixAnswersAndResetsDaily(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "empty")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	gate := func() *ClientGate { return &ClientGate{now: func() time.Time { return now }} }

	probe(gate(), s, "daemon", SurfaceTUI)
	probe(gate(), s, "daemon", SurfaceTUI)
	assert.Len(t, s.seen(), 1, "a new process waits out the saved backoff")

	for want := 2; want <= maxProbeRejections; want++ {
		now = now.Add(time.Minute)
		probe(gate(), s, "daemon", SurfaceTUI)
		assert.Len(t, s.seen(), want)
	}
	now = now.Add(time.Hour)
	probe(gate(), s, "daemon", SurfaceTUI)
	assert.Len(t, s.seen(), maxProbeRejections, "the probe stops after six answers without a receipt")

	now = now.AddDate(0, 0, 1)
	probe(gate(), s, "daemon", SurfaceTUI)
	assert.Len(t, s.seen(), maxProbeRejections+1)
}

func TestClientGateProbeStopsAfterSixRefusals(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "401")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	for range 2 * maxProbeRejections {
		probe(&ClientGate{now: func() time.Time { return now }}, s, "daemon", SurfaceCLI)
		now = now.Add(time.Minute)
	}
	assert.Len(t, s.seen(), maxProbeRejections)
}

func TestClientGateReportsBackOffAfterAServerError(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "500")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	g := &ClientGate{now: func() time.Time { return now }}

	for range 20 {
		post(g, s, "daemon", EventAgentActive, "")
	}
	assert.Len(t, s.seen(), 1, "calls inside the backoff are dropped")

	now = now.Add(time.Second)
	post(g, s, "daemon", EventAgentActive, "")
	assert.Len(t, s.seen(), 2)
	now = now.Add(time.Second)
	post(g, s, "daemon", EventAgentActive, "")
	assert.Len(t, s.seen(), 2, "the second failure doubles the wait")
	now = now.Add(time.Second)
	post(g, s, "daemon", EventAgentActive, "")
	assert.Len(t, s.seen(), 3)

	s.status.Store("queued")
	now = now.Add(time.Minute)
	post(g, s, "daemon", EventAgentActive, "")
	post(g, s, "daemon", EventAgentActive, "")
	assert.Len(t, s.seen(), 5, "a good answer clears the backoff")
	post(g, s, "other", EventAgentActive, "")
	assert.Len(t, s.seen(), 6, "another daemon is unaffected")
}

func TestClientGateServerErrorsOnlyBackOff(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "500")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	gate := func() *ClientGate { return &ClientGate{now: func() time.Time { return now }} }
	for range 2 * maxProbeRejections {
		probe(gate(), s, "daemon", SurfaceTUI)
		now = now.Add(time.Minute)
	}
	require.Len(t, s.seen(), 2*maxProbeRejections)

	s.status.Store("queued")
	probe(gate(), s, "daemon", SurfaceTUI)
	probe(gate(), s, "daemon", SurfaceTUI)
	assert.Len(t, s.seen(), 2*maxProbeRejections+1, "the daemon recovered, so the check sends once")
}

func TestClientGateUnansweredProbesKeepRetryingAfterBackoff(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "queued")
	s.Close()
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	gate := func() *ClientGate { return &ClientGate{now: func() time.Time { return now }} }
	attempts := func() int {
		data, err := os.ReadFile(filepath.Join(os.Getenv("ROBOREV_DATA_DIR"), clientStateFile))
		require.NoError(t, err)
		var file struct {
			Endpoints map[string]struct {
				Probes map[string]clientProbe `json:"probes"`
			} `json:"endpoints"`
		}
		require.NoError(t, json.Unmarshal(data, &file))
		return file.Endpoints["daemon"].Probes[SurfaceCLI].Attempts
	}

	for want := 1; want <= 3*maxProbeRejections; want++ {
		probe(gate(), s, "daemon", SurfaceCLI)
		assert.Equal(t, want, attempts())
		probe(gate(), s, "daemon", SurfaceCLI)
		assert.Equal(t, want, attempts(), "backoff holds the next try")
		now = now.Add(17 * time.Second)
	}

	revived := newReceiptServer(t, "queued")
	probe(gate(), revived, "daemon", SurfaceCLI)
	assert.Equal(t, []string{EventAppOpened}, revived.seen())
}

func TestClientGateExhaustedProbeStopsOnlyItself(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "empty")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	g := &ClientGate{now: func() time.Time { return now }}

	for range maxProbeRejections {
		probe(g, s, "daemon", SurfaceTUI)
		now = now.Add(time.Minute)
	}
	s.status.Store("queued")
	post(g, s, "daemon", EventScreenViewed, SurfaceTUI)
	assert.Equal(t, EventScreenViewed, s.seen()[len(s.seen())-1])
}

func TestClientGateFollowerSeesTheSentReceipt(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "queued")
	probe(&ClientGate{}, s, "daemon", SurfaceCLI)

	follower := &ClientGate{}
	probe(follower, s, "daemon", SurfaceCLI)
	post(follower, s, "daemon", EventReviewClosed, SurfaceCLI)
	assert.Equal(t, []string{EventAppOpened, EventReviewClosed}, s.seen())
}

func TestClientGateKeepsOnlyDisabledAndProbeState(t *testing.T) {
	dir := testenv.SetDataDir(t)
	s := newReceiptServer(t, "queued")
	g := &ClientGate{}
	probe(g, s, "daemon", SurfaceCLI)
	for _, event := range []string{EventReviewViewed, EventReviewClosed, EventFindingFixed, EventAgentActive} {
		post(g, s, "daemon", event, SurfaceCLI)
	}
	post(g, s, "daemon", EventSessionEnded, SurfaceCLI)

	data, err := os.ReadFile(filepath.Join(dir, clientStateFile))
	require.NoError(t, err)
	var file struct {
		Day       string                    `json:"day"`
		Endpoints map[string]map[string]any `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal(data, &file))
	require.Len(t, file.Endpoints, 1)
	for key := range file.Endpoints["daemon"] {
		assert.Contains(t, []string{"disabled", "probes"}, key)
	}
	assert.Contains(t, string(data), `"sent":true`)
	assert.NotContains(t, string(data), EventReviewViewed)
}

func TestClientGateUnusableStorageKeepsReceiptsInMemory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	t.Setenv("ROBOREV_DATA_DIR", filepath.Join(blocker, "data"))
	s := newReceiptServer(t, "disabled")
	g := &ClientGate{}

	probe(g, s, "daemon", SurfaceCLI)
	probe(g, s, "daemon", SurfaceCLI)
	post(g, s, "daemon", EventAgentActive, "")
	assert.Equal(t, []string{EventAppOpened}, s.seen())
	assert.False(t, g.Ready("daemon"))
}

func TestClientGateReadsUnreadableFileAsEmpty(t *testing.T) {
	dir := testenv.SetDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, clientStateFile), []byte("{not json"), 0o600))
	s := newReceiptServer(t, "queued")

	probe(&ClientGate{}, s, "daemon", SurfaceCLI)
	probe(&ClientGate{}, s, "daemon", SurfaceCLI)
	assert.Equal(t, []string{EventAppOpened}, s.seen())
}

func TestClientGateGivesUpWhenTheLockStaysBusy(t *testing.T) {
	dir := testenv.SetDataDir(t)
	held := flock.New(filepath.Join(dir, clientStateFile+".lock"))
	locked, err := held.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { _ = held.Unlock() })

	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("unexpected request")
		})}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		g := &ClientGate{}
		g.Probe(ctx, client, "daemon", "http://daemon.test", SurfaceCLI)
		assert.Zero(t, requests.Load())
		assert.True(t, g.Ready("daemon"))
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestClientGateRollsOverAtUTCMidnight(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "disabled")
	now := time.Date(2026, 3, 10, 23, 59, 0, 0, time.UTC)
	g := &ClientGate{now: func() time.Time { return now }}
	probe(g, s, "daemon", SurfaceCLI)
	assert.False(t, g.Ready("daemon"))

	now = now.Add(2 * time.Minute)
	assert.True(t, g.Ready("daemon"))
}

func TestClientGateFollowerWaitsForTheProbeInFlight(t *testing.T) {
	testenv.SetDataDir(t)
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var requests atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			<-release
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Body:       io.NopCloser(strings.NewReader(`{"status":"queued"}`)),
				Header:     make(http.Header),
			}, nil
		})}
		run := func() {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			(&ClientGate{}).Probe(ctx, client, "daemon", "http://daemon.test", SurfaceTUI)
		}
		var first, follower sync.WaitGroup
		first.Go(run)
		synctest.Wait()
		followed := false
		follower.Go(func() {
			run()
			followed = true
		})
		time.Sleep(time.Second)
		synctest.Wait()
		assert.False(t, followed, "the follower waits while the first check is in flight")

		close(release)
		first.Wait()
		follower.Wait()
		assert.Equal(t, int32(1), requests.Load())
	})
}

func TestClientGateRefusesANewerFileAndLeavesItIntact(t *testing.T) {
	for name, content := range map[string]string{
		"same shape":    `{"version":2,"day":"2026-03-10","endpoints":{"daemon":{"disabled":true}}}`,
		"changed shape": `{"version":2,"day":"2026-03-10","endpoints":[{"scope":"daemon"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := testenv.SetDataDir(t)
			now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
			path := filepath.Join(dir, clientStateFile)
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			s := newReceiptServer(t, "disabled")
			g := &ClientGate{now: func() time.Time { return now }}

			probe(g, s, "daemon", SurfaceCLI)
			post(g, s, "daemon", EventAgentActive, "")
			post(g, s, "daemon", EventReviewViewed, SurfaceCLI)
			assert.Empty(t, s.seen(), "a format this build can't read gets no reports")
			assert.False(t, g.Ready("daemon"))
			saved, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, content, string(saved))
		})
	}
}

func TestClientGateReportsHonorTheSavedProbeBackoff(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "500")
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	gate := func() *ClientGate { return &ClientGate{now: func() time.Time { return now }} }
	probe(gate(), s, "daemon", SurfaceCLI)
	require.Len(t, s.seen(), 1)

	fresh := gate()
	probe(fresh, s, "daemon", SurfaceCLI)
	post(fresh, s, "daemon", EventReviewViewed, SurfaceCLI)
	assert.Len(t, s.seen(), 1, "the saved backoff covers dependent reports")

	now = now.Add(2 * time.Second)
	post(fresh, s, "daemon", EventReviewViewed, SurfaceCLI)
	assert.Len(t, s.seen(), 2)
}
