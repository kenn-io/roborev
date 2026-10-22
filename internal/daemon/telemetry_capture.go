package daemon

import (
	"context"
	"encoding/json"
	"mime"
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
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser origin is forbidden", http.StatusForbidden)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "application/json is required", http.StatusUnsupportedMediaType)
			return
		}
		// Clients stop waiting shortly after exit, so their disconnect must not drop the count.
		status, err := s.recordAgentCall(context.WithoutCancel(r.Context()))
		if err != nil {
			http.Error(w, "capture telemetry event failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": string(status)})
	})
}

// handleTelemetryEvent reads the reporter per request because SetTelemetry runs after route registration.
// Capture keeps its own deadline because CLI and TUI clients may exit before the daemon finishes.
func (s *Server) handleTelemetryEvent(w http.ResponseWriter, r *http.Request) {
	reporter, _ := s.telemetry.(*telemetry.Reporter)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), telemetry.NotificationTimeout)
	defer cancel()
	s.appOpened.Handler(reporter).ServeHTTP(w, r.WithContext(ctx))
}
