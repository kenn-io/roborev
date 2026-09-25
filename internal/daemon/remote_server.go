package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
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
	addrPort, err := netip.ParseAddrPort(remote.Listen)
	if err != nil {
		return fmt.Errorf(
			"remote_api.listen %q must be a Tailscale IP and port, such as 100.101.102.103:7474: %w",
			remote.Listen, err)
	}
	if !remoteListenAddrAllowed(addrPort.Addr()) {
		return fmt.Errorf(
			"remote_api.listen %q is not a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48); set it to this host's Tailscale IP",
			remote.Listen)
	}
	if addrPort.Port() == 0 {
		return fmt.Errorf("remote_api.listen %q needs a fixed port", remote.Listen)
	}
	listener, err := net.Listen("tcp", remote.Listen)
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
