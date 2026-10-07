package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
)

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
	_, err := db.Exec(`CREATE TABLE agent_count_writes (value TEXT); CREATE TRIGGER audit_agent_count_update AFTER UPDATE ON sync_state BEGIN INSERT INTO agent_count_writes VALUES (NEW.value); END`)
	require.NoError(t, err)
	for range 3 {
		server.recordAgentCall(t.Context())
	}
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	var saturated agentActivityState
	require.NoError(t, json.Unmarshal([]byte(stored), &saturated))
	assert.Equal(uint64(101), saturated.Calls)
	var writes int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM agent_count_writes`).Scan(&writes))
	assert.Zero(writes)
	_, err = db.Exec(`DROP TRIGGER audit_agent_count_update`)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	server.recordAgentCall(t.Context())
	assert.Len(client.events, 4)
	assert.Equal(telemetry.EventAgentActive, client.events[3])
	stored, err = db.GetSyncState(agentActivityKey)
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

func TestAgentActivityDatabaseCancellation(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.SetTelemetry(&fakeTelemetryClient{enabled: true})
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.recordAgentCall(ctx)
	}()
	// Wall-clock wait: database/sql waits for the held SQLite connection.
	require.Eventually(t, func() bool { return db.Stats().WaitCount > 0 }, time.Second, time.Millisecond)
	cancel()
	<-done
	require.NoError(t, conn.Close())
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestAgentActivityBusyWriteCancellation(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.SetTelemetry(&fakeTelemetryClient{enabled: true})
	writer, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	_, err = writer.Exec(`INSERT INTO sync_state (key, value) VALUES ('writer-lock', 'held')`)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.recordAgentCall(t.Context())
	}()
	// Wall-clock wait: SQLite's busy writer must honor the notification deadline.
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 2*telemetry.NotificationTimeout, time.Millisecond)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	var busyTimeout int
	require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, 30000, busyTimeout)
	require.NoError(t, conn.Close())
	require.NoError(t, writer.Rollback())
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

type restoreFailureConnector struct {
	driver driver.Driver
	dsn    string
}

func (c restoreFailureConnector) Driver() driver.Driver { return c.driver }

func (c restoreFailureConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &restoreFailureConn{Conn: conn}, nil
}

type restoreFailureConn struct {
	driver.Conn
	restoreFailed bool
	closed        bool
}

func (c *restoreFailureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if query == "PRAGMA busy_timeout = 30000" {
		c.restoreFailed = true
		return nil, errors.New("timeout restore failed")
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *restoreFailureConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func TestAgentActivityFailedRestoreDiscardsConnection(t *testing.T) {
	server, db, _ := newTestServer(t)
	var path string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	pool := sql.OpenDB(restoreFailureConnector{driver: db.Driver(), dsn: path + "?_pragma=busy_timeout(30000)"})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	pool.SetMaxOpenConns(1)
	server.db = &storage.DB{DB: pool}
	server.SetTelemetry(&fakeTelemetryClient{enabled: true})
	conn, err := pool.Conn(t.Context())
	require.NoError(t, err)
	var affected *restoreFailureConn
	require.NoError(t, conn.Raw(func(raw any) error {
		affected = raw.(*restoreFailureConn)
		return nil
	}))
	require.NoError(t, conn.Close())
	server.recordAgentCall(t.Context())
	assert.True(t, affected.restoreFailed)
	assert.True(t, affected.closed)
	var busyTimeout int
	require.NoError(t, pool.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, 30000, busyTimeout)
	require.NoError(t, server.db.SetSyncState("ordinary-write", "saved"))
}
