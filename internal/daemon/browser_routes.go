package daemon

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

type WebLoginRequest struct {
	Token string `json:"token" minLength:"1"`
}

type WebSessionCredentials struct {
	Session      string                 `json:"session"`
	CSRF         string                 `json:"csrf"`
	ExpiresAt    time.Time              `json:"expires_at"`
	Capabilities WebSessionCapabilities `json:"capabilities"`
}

type WebSessionCapabilities struct {
	CancelAnyJob    bool `json:"cancel_any_job"`
	CancelReviewJob bool `json:"cancel_review_job"`
	RerunJob        bool `json:"rerun_job"`
}

type WebSessionStatus struct {
	Authentication string                  `json:"authentication" enum:"local,token,proxy"`
	Authenticated  bool                    `json:"authenticated"`
	ExpiresAt      *time.Time              `json:"expires_at,omitempty"`
	Capabilities   *WebSessionCapabilities `json:"capabilities,omitempty"`
}

type WebSessionError struct {
	Error string `json:"error"`
}

type WebLoginInput struct {
	Body WebLoginRequest
}

type WebBootstrapInput struct {
	Body struct{}
}

type WebSessionCredentialsOutput struct {
	Body WebSessionCredentials
}

type WebSessionStatusOutput struct {
	Body WebSessionStatus
}

type WebLogoutOutput struct{}

func (s *Server) registerBrowserRoutes(api huma.API) {
	huma.Post(api, "/api/ui/session/login", unavailableWebLogin,
		func(operation *huma.Operation) {
			operation.OperationID = "login-web-session"
			operation.Summary = "Exchange a daemon token for a browser session"
			operation.Tags = []string{"web-session"}
			setWebSessionErrorResponses(api, operation, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusUnsupportedMediaType)
		})
	huma.Post(api, "/api/ui/session/bootstrap", unavailableWebBootstrap,
		func(operation *huma.Operation) {
			operation.OperationID = "bootstrap-web-session"
			operation.Summary = "Mint tab credentials from an ambient browser session"
			operation.Tags = []string{"web-session"}
			setWebSessionErrorResponses(api, operation, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnsupportedMediaType)
		})
	huma.Delete(api, "/api/ui/session", unavailableWebLogout,
		func(operation *huma.Operation) {
			operation.OperationID = "logout-web-session"
			operation.Summary = "Invalidate a browser session"
			operation.Tags = []string{"web-session"}
			operation.DefaultStatus = 204
			setWebSessionErrorResponses(api, operation, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden)
		})
	huma.Get(api, "/api/ui/session", unavailableWebSessionStatus,
		func(operation *huma.Operation) {
			operation.OperationID = "get-web-session-status"
			operation.Summary = "Get browser authentication status"
			operation.Tags = []string{"web-session"}
			setWebSessionErrorResponses(api, operation, http.StatusBadRequest)
		})
}

func setWebSessionErrorResponses(api huma.API, operation *huma.Operation, statuses ...int) {
	if operation.Responses == nil {
		operation.Responses = make(map[string]*huma.Response)
	}
	schema := jsonSchema(api, WebSessionError{})
	for _, status := range statuses {
		operation.Responses[strconv.Itoa(status)] = &huma.Response{
			Description: http.StatusText(status),
			Content: map[string]*huma.MediaType{
				"application/json": {Schema: schema},
			},
		}
	}
}

func unavailableWebLogin(context.Context, *WebLoginInput) (*WebSessionCredentialsOutput, error) {
	return nil, huma.Error404NotFound("browser session routes require the browser listener")
}

func unavailableWebBootstrap(context.Context, *WebBootstrapInput) (*WebSessionCredentialsOutput, error) {
	return nil, huma.Error404NotFound("browser session routes require the browser listener")
}

func unavailableWebLogout(context.Context, *struct{}) (*WebLogoutOutput, error) {
	return nil, huma.Error404NotFound("browser session routes require the browser listener")
}

func unavailableWebSessionStatus(context.Context, *struct{}) (*WebSessionStatusOutput, error) {
	return nil, huma.Error404NotFound("browser session routes require the browser listener")
}
