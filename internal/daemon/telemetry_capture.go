package daemon

import (
	"net/http"

	"go.kenn.io/roborev/internal/telemetry"
)

// TelemetryEventsPath is the core-listener route the web UI, the TUI and the CLI post product events to.
const TelemetryEventsPath = "/api/telemetry/events"

// TelemetryAgentCallPath accepts core-only notifications; the daemon owns the count.
const TelemetryAgentCallPath = "/api/telemetry/agent-call"

func (s *Server) registerTelemetryCaptureRoute(mux *http.ServeMux) {
	mux.HandleFunc(http.MethodPost+" "+TelemetryEventsPath, s.handleTelemetryEvent)
	mux.HandleFunc(http.MethodPost+" "+TelemetryAgentCallPath, func(w http.ResponseWriter, r *http.Request) {
		s.recordAgentCall(r.Context())
		w.WriteHeader(http.StatusAccepted)
	})
}

// handleTelemetryEvent reads the reporter per request because SetTelemetry runs after route registration.
func (s *Server) handleTelemetryEvent(w http.ResponseWriter, r *http.Request) {
	reporter, _ := s.telemetry.(*telemetry.Reporter)
	s.appOpened.Handler(reporter).ServeHTTP(w, r)
}
