package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type routeKey struct {
	method string
	path   string
}

type routePolicy int

const (
	publicRoute routePolicy = iota
	authenticatedRoute
	mutationRoute
)

var browserAPIRoutes = map[routeKey]routePolicy{
	{http.MethodGet, "/api/ping"}:                 publicRoute,
	{http.MethodGet, "/api/status"}:               authenticatedRoute,
	{http.MethodGet, "/api/jobs"}:                 authenticatedRoute,
	{http.MethodGet, "/api/review"}:               authenticatedRoute,
	{http.MethodGet, "/api/comments"}:             authenticatedRoute,
	{http.MethodGet, "/api/repos"}:                authenticatedRoute,
	{http.MethodGet, "/api/branches"}:             authenticatedRoute,
	{http.MethodGet, "/api/job/output"}:           authenticatedRoute,
	{http.MethodGet, "/api/job/log"}:              authenticatedRoute,
	{http.MethodGet, "/api/stream/events"}:        authenticatedRoute,
	{http.MethodGet, "/api/ui/review-projection"}: authenticatedRoute,
	{http.MethodGet, "/api/ui/analytics"}:         authenticatedRoute,
	{http.MethodPost, "/api/job/cancel"}:          mutationRoute,
	{http.MethodPost, "/api/job/rerun"}:           mutationRoute,
	{http.MethodPost, "/api/review/close"}:        mutationRoute,
	{http.MethodPost, "/api/comment"}:             mutationRoute,
}

type browserPrincipalContextKey struct{}

func BrowserPrincipalFromContext(ctx context.Context) (BrowserPrincipal, bool) {
	principal, found := ctx.Value(browserPrincipalContextKey{}).(BrowserPrincipal)
	return principal, found
}

func (s *Server) newBrowserHandler(
	core http.Handler,
	static http.Handler,
	policy BrowserPolicy,
	sessions *BrowserSessionManager,
) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := policy.ValidateHost(request); err != nil {
			writeBrowserError(w, http.StatusBadRequest, "invalid_host")
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/ui/session") {
			handleBrowserSession(w, request, policy, sessions)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/debug/") || isBrowserOpenAPIPath(request.URL.Path) {
			route, found := browserAPIRoutes[routeKey{request.Method, request.URL.Path}]
			if !found {
				http.NotFound(w, request)
				return
			}
			if route == publicRoute {
				core.ServeHTTP(w, request)
				return
			}
			principal, ambient, tab, err := authenticateBrowserRequest(request, sessions)
			if err != nil {
				writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
				return
			}
			if route == mutationRoute {
				if err := policy.ValidateOrigin(request); err != nil {
					writeBrowserError(w, http.StatusForbidden, "invalid_origin")
					return
				}
				if err := sessions.CheckCSRF(tab, request.Header.Get(WebCSRFHeader)); err != nil {
					writeBrowserError(w, http.StatusForbidden, "csrf_invalid")
					return
				}
			}
			_ = ambient
			w.Header().Set("Cache-Control", "private, no-store")
			w.Header().Add("Vary", "Cookie")
			w.Header().Add("Vary", WebSessionHeader)
			core.ServeHTTP(w, request.WithContext(context.WithValue(request.Context(), browserPrincipalContextKey{}, principal)))
			return
		}
		static.ServeHTTP(w, request)
	}), nil
}

func handleBrowserSession(w http.ResponseWriter, request *http.Request, policy BrowserPolicy, sessions *BrowserSessionManager) {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/api/ui/session/login":
		if err := policy.ValidateOrigin(request); err != nil {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		var login WebLoginRequest
		if !readBrowserJSON(w, request, &login) {
			return
		}
		credentials, err := sessions.Login(login.Token)
		if err != nil {
			var limited *WebLoginRateLimitError
			if errors.As(err, &limited) {
				seconds := max(1, int((limited.RetryAfter+time.Second-1)/time.Second))
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeBrowserError(w, http.StatusTooManyRequests, "login_rate_limited")
				return
			}
			writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
			return
		}
		http.SetCookie(w, sessions.Cookie(credentials.Ambient))
		writeBrowserCredentials(w, credentials)
	case request.Method == http.MethodPost && request.URL.Path == "/api/ui/session/bootstrap":
		if err := policy.ValidateOrigin(request); err != nil {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		if request.Header.Get("Sec-Fetch-Site") != "same-origin" || request.Header.Get("Sec-Fetch-Mode") != "cors" || request.Header.Get("Sec-Fetch-Dest") != "empty" {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
			writeBrowserError(w, http.StatusUnsupportedMediaType, "invalid_request")
			return
		}
		cookie, err := request.Cookie(sessions.CookieName())
		var credentials SessionCredentials
		if err == nil {
			credentials, err = sessions.Bootstrap(cookie.Value)
		} else if policy.AllowsLocalSession(request) {
			credentials, err = sessions.NewLocalSession()
			if err == nil {
				http.SetCookie(w, sessions.Cookie(credentials.Ambient))
			}
		} else {
			err = ErrWebSessionRequired
		}
		if err != nil {
			writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
			return
		}
		writeBrowserCredentials(w, credentials)
	case request.Method == http.MethodDelete && request.URL.Path == "/api/ui/session":
		if err := policy.ValidateOrigin(request); err != nil {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		_, ambient, tab, err := authenticateBrowserRequest(request, sessions)
		if err != nil {
			writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
			return
		}
		if err := sessions.CheckCSRF(tab, request.Header.Get(WebCSRFHeader)); err != nil {
			writeBrowserError(w, http.StatusForbidden, "csrf_invalid")
			return
		}
		sessions.Logout(ambient)
		http.SetCookie(w, sessions.ExpiredCookie())
		w.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && request.URL.Path == "/api/ui/session":
		writeBrowserSessionStatus(w, request, policy, sessions)
	default:
		http.NotFound(w, request)
	}
}

func authenticateBrowserRequest(request *http.Request, sessions *BrowserSessionManager) (BrowserPrincipal, string, string, error) {
	cookie, err := request.Cookie(sessions.CookieName())
	if err != nil {
		return BrowserPrincipal{}, "", "", ErrWebSessionRequired
	}
	tab := request.Header.Get(WebSessionHeader)
	principal, err := sessions.Authenticate(cookie.Value, tab)
	return principal, cookie.Value, tab, err
}

func writeBrowserSessionStatus(w http.ResponseWriter, request *http.Request, policy BrowserPolicy, sessions *BrowserSessionManager) {
	authentication := "local"
	if policy.remoteAuthEnabled {
		authentication = "token"
	}
	status := WebSessionStatus{Authentication: authentication}
	_, ambient, tab, err := authenticateBrowserRequest(request, sessions)
	if err == nil {
		status.Authenticated = true
		if expiry, expiryErr := sessions.SessionExpiry(ambient, tab); expiryErr == nil {
			status.ExpiresAt = &expiry
		}
	}
	writeBrowserJSON(w, http.StatusOK, status)
}

func writeBrowserCredentials(w http.ResponseWriter, credentials SessionCredentials) {
	writeBrowserJSON(w, http.StatusOK, WebSessionCredentials{
		Session: credentials.Tab, CSRF: credentials.CSRF, ExpiresAt: credentials.Expires,
	})
}

func readBrowserJSON(w http.ResponseWriter, request *http.Request, destination any) bool {
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		writeBrowserError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeBrowserError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func writeBrowserJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeBrowserError(w http.ResponseWriter, status int, code string) {
	writeBrowserJSON(w, status, map[string]string{"error": code})
}

func isBrowserOpenAPIPath(path string) bool {
	return path == "/openapi.json" || path == "/openapi.yaml" || strings.HasPrefix(path, "/schemas/")
}
