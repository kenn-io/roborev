package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"go.kenn.io/roborev/internal/config"
	webassets "go.kenn.io/roborev/internal/web"
)

var browserCapabilities = []string{"web-ui-v1", "web-session-v1", "analytics-v1"}

func (s *Server) startBrowserServer(web config.WebConfig) (*BrowserRuntimeInfo, error) {
	if !web.Enabled {
		return nil, nil
	}
	if s.webDevOrigin == "" && !s.allowWebCompilationStub && !webassets.EmbeddedReleaseAvailable() {
		log.Printf("Browser application disabled: this source build does not contain production web assets")
		return nil, nil
	}
	authToken, err := web.ResolveAuthToken()
	if err != nil {
		return nil, err
	}
	endpoint, err := ResolveBrowserEndpoint(web)
	if err != nil {
		return nil, err
	}
	if !endpoint.Enabled {
		return nil, nil
	}
	fail := func(err error) (*BrowserRuntimeInfo, error) {
		_ = endpoint.Listener.Close()
		return nil, err
	}
	policy, err := NewBrowserPolicy(endpoint, s.webDevOrigin)
	if err != nil {
		return fail(err)
	}
	sessionOrigin := endpoint.Origin
	if s.webDevOrigin != "" {
		sessionOrigin = s.webDevOrigin
	}
	sessions, err := NewBrowserSessionManager(BrowserSessionConfig{
		Origin:     sessionOrigin,
		AuthToken:  authToken,
		AllowLocal: endpoint.authentication == "local",
		AllowProxy: endpoint.authentication == "proxy",
		CookiePath: joinBrowserPath(web.BasePath, "/"),
		Entropy:    rand.Reader,
		Clock:      time.Now,
	})
	if err != nil {
		return fail(err)
	}
	static, err := webassets.NewEmbeddedHandler(web.BasePath)
	if err != nil {
		return fail(fmt.Errorf("load embedded browser application: %w", err))
	}
	handler, err := s.newBrowserHandler(s.httpServer.Handler, static, policy, sessions, web.BasePath)
	if err != nil {
		return fail(err)
	}
	requestContext, cancelRequests := context.WithCancel(context.Background())
	server := &http.Server{
		Addr:              endpoint.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext: func(net.Listener) context.Context {
			return requestContext
		},
	}
	server.RegisterOnShutdown(cancelRequests)
	s.browserMu.Lock()
	if s.browserStopping {
		s.browserMu.Unlock()
		cancelRequests()
		return fail(fmt.Errorf("server stopped during browser startup"))
	}
	s.browserServer = server
	s.browserListener = endpoint.Listener
	s.browserMu.Unlock()
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- server.Serve(endpoint.Listener)
	}()
	if err := waitForBrowserReady(endpoint, web.BasePath, serveErrCh); err != nil {
		cancelRequests()
		_ = server.Close()
		return nil, err
	}
	go func() {
		if serveErr := <-serveErrCh; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("Browser HTTP server stopped: %v", serveErr)
		}
	}()
	origin := endpoint.Origin
	if s.webDevOrigin != "" {
		origin = s.webDevOrigin
	}
	return &BrowserRuntimeInfo{
		Address:      endpoint.DialAddress,
		Origin:       origin,
		WebBasePath:  web.BasePath,
		Capabilities: append([]string(nil), browserCapabilities...),
	}, nil
}

func waitForBrowserReady(endpoint BrowserEndpoint, basePath string, serveErrCh <-chan error) error {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-serveErrCh:
			if err == nil {
				return fmt.Errorf("browser server exited before ready")
			}
			return err
		default:
		}
		request, err := http.NewRequest(
			http.MethodGet,
			"http://"+endpoint.DialAddress+joinBrowserPath(basePath, "/api/ping"),
			nil,
		)
		if err != nil {
			return err
		}
		request.Host = endpoint.Address
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("browser server did not become ready")
}
