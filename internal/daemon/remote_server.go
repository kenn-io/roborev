package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// remoteListenAddrAllowed decides which bind hosts the remote listener
// accepts. Tests replace it to bind loopback.
var remoteListenAddrAllowed = config.IsTailscaleAddr

// startRemoteServer serves the core API to tailnet peers on the configured
// Tailscale address. It uses the browser listener's header and idle timeouts
// but no whole-request timeout: pack uploads and event streams can run long.
func (s *Server) startRemoteServer(remote config.RemoteAPIConfig) error {
	if !remote.Enabled {
		return nil
	}
	addrPort, err := config.ValidateRemoteListen(remote.Listen, remoteListenAddrAllowed)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addrPort.String())
	if err != nil {
		return fmt.Errorf("listen on remote_api.listen %s: %w", remote.Listen, err)
	}
	whois := s.remoteWhois
	if whois == nil {
		whois = tailscaleWhois(remote.TailscalePath)
	}
	server := &http.Server{
		Handler:           s.newRemoteHandler(s.httpServer.Handler, whois),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ConnContext:       remoteConnContext,
	}
	s.browserMu.Lock()
	if s.browserStopping {
		s.browserMu.Unlock()
		_ = listener.Close()
		return http.ErrServerClosed
	}
	s.remoteServer = server
	s.browserMu.Unlock()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Remote API server stopped: %v", err)
		}
	}()
	log.Printf("Remote API listening on %s (tailnet identity required)", listener.Addr())
	return nil
}
