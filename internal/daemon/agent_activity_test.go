package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/telemetry"
)

const agentActivityKey = "telemetry.agent_activity"

type agentActivityState struct {
	Day   string `json:"day"`
	Calls uint64 `json:"calls"`
}

func TestAgentActivityBucketsRestartAndUTC(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	now := time.Date(2026, 1, 2, 18, 59, 0, 0, time.FixedZone("offset", -5*60*60))
	server.agentActivityNow = func() time.Time { return now }
	for call := 1; call <= 101; call++ {
		server.recordAgentCall(t.Context())
		if call == 1 || call == 2 || call == 10 {
			assert.Len(client.events, 1, "call %d", call)
		}
		if call == 11 || call == 100 {
			assert.Len(client.events, 2, "call %d", call)
		}
		if call == 10 {
			// A new server reads the same durable state after restart.
			server = &Server{db: db, telemetry: client, agentActivityGate: make(chan struct{}, 1), agentActivityNow: server.agentActivityNow}
		}
	}
	assert.Equal([]string{telemetry.EventAgentActive, telemetry.EventAgentCallCount, telemetry.EventAgentCallCount}, client.events)
	assert.Equal([]map[string]any{
		{telemetry.PropertyCallCountBucket: "1-10"},
		{telemetry.PropertyCallCountBucket: "11-100"},
		{telemetry.PropertyCallCountBucket: "over-100"},
	}, client.properties)
	for range 3 {
		server.recordAgentCall(t.Context())
	}
	assert.Len(client.events, 3)

	now = now.Add(time.Minute)
	server.recordAgentCall(t.Context())
	assert.Len(client.events, 4)
	assert.Equal(telemetry.EventAgentActive, client.events[3])
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	var state agentActivityState
	require.NoError(t, json.Unmarshal([]byte(stored), &state))
	assert.Equal(agentActivityState{Day: "2026-01-03", Calls: 1}, state)
}

func TestAgentActivityConcurrentEntryPoints(t *testing.T) {
	server, db, _ := newTestServer(t)
	db.SetMaxOpenConns(1)
	_, err := db.Exec(`PRAGMA synchronous = OFF`)
	require.NoError(t, err)
	client := &fakeTelemetryClient{enabled: true}
	server.SetTelemetry(client)
	server.agentActivityNow = func() time.Time { return time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) }
	var wg sync.WaitGroup
	for i := range 101 {
		wg.Go(func() {
			if i%2 == 0 {
				server.recordAgentCall(t.Context())
				return
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
			req.Header.Set("Content-Type", "application/json")
			server.httpServer.Handler.ServeHTTP(w, req)
			assert.Equal(t, http.StatusAccepted, w.Code)
		})
	}
	wg.Wait()
	assert.Equal(t, []string{telemetry.EventAgentActive, telemetry.EventAgentCallCount, telemetry.EventAgentCallCount}, client.events)
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	var state agentActivityState
	require.NoError(t, json.Unmarshal([]byte(stored), &state))
	assert.Equal(t, uint64(101), state.Calls)
}

type failingAgentTelemetry struct{ fakeTelemetryClient }

func (f *failingAgentTelemetry) Capture(event string, props map[string]any) error {
	_ = f.fakeTelemetryClient.Capture(event, props)
	return errors.New("delivery failed")
}

func TestAgentActivityOptOutAndDeliveryFailure(t *testing.T) {
	server, db, _ := newTestServer(t)
	client := &failingAgentTelemetry{}
	server.SetTelemetry(client)
	server.recordAgentCall(context.Background())
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
	client.enabled = true
	for range 2 {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
		req.Header.Set("Content-Type", "application/json")
		server.httpServer.Handler.ServeHTTP(w, req)
		assert.Equal(t, http.StatusAccepted, w.Code)
	}
	assert.Equal(t, []string{telemetry.EventAgentActive}, client.events)
}

func TestAgentActivityRoutesRejectBrowserCapture(t *testing.T) {
	server, _, _ := newTestServer(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(t, err)
	server.SetTelemetry(reporter)
	for _, tc := range []struct {
		contentType string
		origin      string
		status      int
	}{
		{"", "", http.StatusUnsupportedMediaType},
		{"text/plain", "", http.StatusUnsupportedMediaType},
		{"application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType},
		{"application/json", "https://example.com", http.StatusForbidden},
		{"application/json", "null", http.StatusForbidden},
		{"application/json; charset=utf-8", "", http.StatusAccepted},
	} {
		req := httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)
		req.Header.Set("Content-Type", tc.contentType)
		req.Header.Set("Origin", tc.origin)
		assert.Equal(t, tc.status, serveTelemetryCapture(server.httpServer.Handler, req).Code)
	}
	for _, event := range []string{telemetry.EventAgentActive, telemetry.EventAgentCallCount, telemetry.EventDaemonActive, telemetry.EventDaemonStarted} {
		w := serveTelemetryCapture(server.httpServer.Handler, newTelemetryCaptureRequest(http.MethodPost, []byte(`{"event":"`+event+`"}`)))
		assert.Equal(t, http.StatusBadRequest, w.Code)
	}
	handler, sessions := newBrowserHandlerFixtureWithCore(t, testBrowserAuthToken, server.httpServer.Handler)
	credentials, err := sessions.Login(testBrowserAuthToken)
	require.NoError(t, err)
	req := browserRequest(http.MethodPost, TelemetryAgentCallPath, nil)
	req.AddCookie(sessions.Cookie(credentials.Ambient))
	req.Header.Set(WebSessionHeader, credentials.Tab)
	req.Header.Set(WebCSRFHeader, credentials.CSRF)
	assert.Equal(t, http.StatusNotFound, serveTelemetryCapture(handler, req).Code)
	server.httpServer.Handler = withAuthentication(server.httpServer.Handler, "test-auth-key")
	assert.Equal(t, http.StatusUnauthorized, serveTelemetryCapture(server.httpServer.Handler, httptest.NewRequest(http.MethodPost, TelemetryAgentCallPath, nil)).Code)
}

func TestAgentActivityCanceledWhileWaiting(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.SetTelemetry(&fakeTelemetryClient{enabled: true})
	synctest.Test(t, func(t *testing.T) {
		server.agentActivityGate = make(chan struct{}, 1)
		server.agentActivityGate <- struct{}{}
		go server.recordAgentCall(t.Context())
		synctest.Wait()
		time.Sleep(telemetry.NotificationTimeout)
		synctest.Wait()
		<-server.agentActivityGate
	})
	server.agentActivityGate = make(chan struct{}, 1)
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	server.recordAgentCall(ctx)
	stored, err = db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
}
