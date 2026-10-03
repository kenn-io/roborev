package daemon

import (
	"net/http"

	"go.kenn.io/roborev/internal/telemetry"
)

const telemetryEventsPath = "/api/telemetry/events"

func (s *Server) registerTelemetryCaptureRoute(mux *http.ServeMux) {
	mux.HandleFunc(http.MethodPost+" "+telemetryEventsPath, s.handleTelemetryEvent)
}

// handleTelemetryEvent reads the reporter per request because SetTelemetry runs after route registration.
func (s *Server) handleTelemetryEvent(w http.ResponseWriter, r *http.Request) {
	reporter, _ := s.telemetry.(*telemetry.Reporter)
	telemetry.NewCaptureHandler(reporter).ServeHTTP(w, r)
}
