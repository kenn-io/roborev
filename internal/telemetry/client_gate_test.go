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
	g.Post(ctx, s.Client(), scope, s.URL, event, props)
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

func TestClientGateConcurrentAgentCallsShareDisabledReceipt(t *testing.T) {
	testenv.SetDataDir(t)
	s := newReceiptServer(t, "disabled")
	start := make(chan struct{})
	var calls sync.WaitGroup
	for range 100 {
		g := &ClientGate{}
		g.Load(t.Context())
		calls.Go(func() {
			<-start
			post(g, s, "daemon", EventAgentActive, "")
		})
	}
	close(start)
	calls.Wait()
	assert.Equal(t, []string{EventAgentActive}, s.seen(), "concurrent agent calls share the first disabled receipt")
}

func TestClientGateProbeBackoffPersistsStopsAtSixAnswersAndResetsDaily(t *testing.T) {
	for _, answer := range []string{"empty", "401"} {
		t.Run(answer, func(t *testing.T) {
			testenv.SetDataDir(t)
			s := newReceiptServer(t, answer)
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

			s.status.Store("queued")
			post(gate(), s, "daemon", EventScreenViewed, SurfaceTUI)
			assert.Equal(t, EventScreenViewed, s.seen()[len(s.seen())-1], "an exhausted probe stops only itself")

			now = now.AddDate(0, 0, 1)
			probe(gate(), s, "daemon", SurfaceTUI)
			assert.Len(t, s.seen(), maxProbeRejections+2)
		})
	}
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

func TestClientGateFailuresWithoutAReceiptOnlyBackOff(t *testing.T) {
	for name, tc := range map[string]struct {
		fail   func(*receiptServer)
		revive func(*testing.T, *receiptServer) *receiptServer
	}{
		"server error": {func(*receiptServer) {}, func(_ *testing.T, s *receiptServer) *receiptServer {
			s.status.Store("queued")
			return s
		}},
		"no answer": {(*receiptServer).Close, func(t *testing.T, _ *receiptServer) *receiptServer {
			return newReceiptServer(t, "queued")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			testenv.SetDataDir(t)
			s := newReceiptServer(t, "500")
			tc.fail(s)
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
				return file.Endpoints["daemon"].Probes[SurfaceTUI].Attempts
			}

			for want := 1; want <= 2*maxProbeRejections; want++ {
				probe(gate(), s, "daemon", SurfaceTUI)
				assert.Equal(t, want, attempts())
				probe(gate(), s, "daemon", SurfaceTUI)
				assert.Equal(t, want, attempts(), "backoff holds the next try")
				now = now.Add(17 * time.Second)
			}

			recovered := tc.revive(t, s)
			before := len(recovered.seen())
			probe(gate(), recovered, "daemon", SurfaceTUI)
			probe(gate(), recovered, "daemon", SurfaceTUI)
			assert.Len(t, recovered.seen(), before+1, "the daemon recovered, so the check sends once")
		})
	}
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

func TestClientGateFollowerWaitsWithUnusableStorage(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	t.Setenv("ROBOREV_DATA_DIR", filepath.Join(blocker, "data"))
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var requests atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			<-release
			return &http.Response{
				StatusCode: http.StatusAccepted,
				Body:       io.NopCloser(strings.NewReader(`{"status":"disabled"}`)),
				Header:     make(http.Header),
			}, nil
		})}
		g := &ClientGate{}
		run := func() {
			g.Probe(t.Context(), client, "daemon", "http://daemon.test", SurfaceTUI)
		}
		var first, follower sync.WaitGroup
		first.Go(run)
		synctest.Wait()
		followed := false
		follower.Go(func() {
			run()
			followed = true
		})
		synctest.Wait()
		assert.False(t, followed, "the follower waits for the receipt even without a file lock")
		close(release)
		first.Wait()
		follower.Wait()
		assert.Equal(t, int32(1), requests.Load())
		assert.False(t, g.Ready("daemon"))
	})
}

func TestClientGateRefusesANewerFileAndLeavesItIntact(t *testing.T) {
	for name, content := range map[string]string{
		"same shape":    `{"version":2,"day":"2026-03-10","endpoints":{}}`,
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

func TestClientGateReceiptAcrossMidnightCountsForTheDayItWasSentOn(t *testing.T) {
	testenv.SetDataDir(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		clock.Store(time.Date(2026, 3, 11, 0, 0, 1, 0, time.UTC).UnixNano())
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	}))
	t.Cleanup(srv.Close)
	ctx := t.Context()

	(&ClientGate{now: now}).Probe(ctx, srv.Client(), "daemon", srv.URL, SurfaceCLI)
	(&ClientGate{now: now}).Probe(ctx, srv.Client(), "daemon", srv.URL, SurfaceCLI)
	assert.Equal(t, int32(2), requests.Load(), "the new day still needs its own check")
}

func TestClientGateNeverRewritesAFileThatTurnedNewerMidReport(t *testing.T) {
	dir := testenv.SetDataDir(t)
	path := filepath.Join(dir, clientStateFile)
	newer := `{"version":2,"day":"2026-03-10","endpoints":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = os.WriteFile(path, []byte(newer), 0o600)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"disabled"}`))
	}))
	t.Cleanup(srv.Close)
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	g := &ClientGate{now: func() time.Time { return now }}

	// A report outside the day's check holds no lock through its request, so another process can write meanwhile.
	g.Post(t.Context(), srv.Client(), "daemon", srv.URL, EventReviewViewed, nil)
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, newer, string(saved))
	assert.False(t, g.Ready("daemon"))
}

func TestClientGateLateDisabledReportLeavesNextDayReady(t *testing.T) {
	testenv.SetDataDir(t)
	now := time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC)
	g := &ClientGate{now: func() time.Time { return now }}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		now = now.Add(2 * time.Second)
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"disabled"}`)), Header: make(http.Header)}, nil
	})}
	g.Post(t.Context(), client, "daemon", "http://daemon.test", EventAgentActive, nil)
	assert.True(t, g.Ready("daemon"))
	var requests int
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(`{"status":"queued"}`)), Header: make(http.Header)}, nil
	})
	(&ClientGate{now: func() time.Time { return now }}).Probe(t.Context(), client, "daemon", "http://daemon.test", SurfaceCLI)
	assert.Equal(t, 1, requests, "a fresh process still checks the new day")
}

func TestClientGateProbeBackoffSkipsBusyLock(t *testing.T) {
	dir := testenv.SetDataDir(t)
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})}
		g := &ClientGate{}
		g.Probe(t.Context(), client, "daemon", "http://daemon.test", SurfaceTUI)
		held := flock.New(filepath.Join(dir, clientStateFile+".lock"))
		require.NoError(t, held.Lock())
		defer func() { require.NoError(t, held.Unlock()) }()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		g.Probe(ctx, client, "daemon", "http://daemon.test", SurfaceTUI)
		assert.NoError(t, ctx.Err(), "backoff returns before waiting on the file lock")
	})
}
