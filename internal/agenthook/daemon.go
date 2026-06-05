package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/version"
)

func RunDaemon(addr string, stderr io.Writer) error {
	if err := assertNoLiveDaemonRecords(); err != nil {
		return err
	}
	ep, err := parseDaemonEndpoint(addr)
	if err != nil {
		return err
	}
	if ep.IsUnix() {
		if err := os.MkdirAll(filepath.Dir(ep.Address), 0o700); err != nil {
			return fmt.Errorf("create agent hook socket dir: %w", err)
		}
		if err := os.Remove(ep.Address); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale agent hook socket: %w", err)
		}
	}

	listener, err := ep.Listen()
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	if ep.IsUnix() {
		defer os.Remove(ep.Address)
	} else {
		ep.Address = listener.Addr().String()
	}

	runtimePath, err := WriteRuntime(ep)
	if err != nil {
		return fmt.Errorf("write agent hook runtime: %w", err)
	}
	defer os.Remove(runtimePath)

	state, err := LoadState()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	shutdown := make(chan struct{}, 1)
	registerRoutes(mux, state, shutdown)
	server := &http.Server{Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var reason string
	select {
	case sig := <-sigCh:
		reason = sig.String()
	case <-shutdown:
		reason = "shutdown request"
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
	fmt.Fprintf(stderr, "%s daemon stopped after %s\n", ServiceName, reason)
	return nil
}

func parseDaemonEndpoint(raw string) (kitdaemon.Endpoint, error) {
	defaultUnixPath := ""
	if path := kitdaemon.DefaultSocketPath(ServiceName); runtime.GOOS != "windows" && path != "" {
		defaultUnixPath = path
	}
	return kitdaemon.ParseEndpoint(raw, kitdaemon.ParseEndpointOptions{
		DefaultTCPAddress: "127.0.0.1:0",
		DefaultUnixPath:   defaultUnixPath,
		TCPPolicy:         kitdaemon.RequireLoopback,
	})
}

func registerRoutes(mux *http.ServeMux, state *StateStore, shutdown chan<- struct{}) {
	mux.Handle(kitdaemon.DefaultPingPath, kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{
		Service: ServiceName,
		Version: version.Version,
		PID:     os.Getpid(),
	}))
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		select {
		case shutdown <- struct{}{}:
		default:
		}
	})
	mux.HandleFunc("/api/hook", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "decode hook request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Event.SessionID == "" {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}
		resp, err := state.Record(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		ids := make([]string, 0, len(state.sessions))
		for id := range state.sessions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := map[string]map[string]SessionState{
			"sessions": make(map[string]SessionState, len(ids)),
		}
		for _, id := range ids {
			out["sessions"][id] = state.sessions[id]
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			All       bool   `json:"all,omitempty"`
			SessionID string `json:"session_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "decode reset request: "+err.Error(), http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		switch {
		case req.All:
			state.sessions = map[string]SessionState{}
		case strings.TrimSpace(req.SessionID) != "":
			delete(state.sessions, req.SessionID)
		default:
			http.Error(w, "missing session_id or all", http.StatusBadRequest)
			return
		}
		if err := state.saveLocked(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
