package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/roborev/pkg/client/generated"
)

var (
	ErrKataAuthentication = errors.New("kata authentication required")
	ErrKataAccess         = errors.New("kata access denied")
)

// ClassifyKataContextError preserves fatal read classes across daemon HTTP.
func ClassifyKataContextError(err error) error {
	if err == nil {
		return nil
	}
	var status int
	if apiErr, ok := errors.AsType[*runtime.ClientAPIError](err); ok && apiErr.StatusCode() != 0 {
		status = apiErr.StatusCode()
	} else if problem, ok := errors.AsType[generated.ErrorModel](err); ok && problem.Status != nil {
		status = int(*problem.Status)
	} else {
		return err
	}
	var class error
	switch status {
	case http.StatusUnauthorized:
		class = ErrKataAuthentication
	case http.StatusForbidden:
		class = ErrKataAccess
	case http.StatusRequestTimeout:
		class = context.Canceled
	case http.StatusGatewayTimeout:
		class = context.DeadlineExceeded
	default:
		return err
	}
	return fmt.Errorf("%w: %w", class, err)
}
