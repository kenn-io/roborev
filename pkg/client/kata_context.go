package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	var problem generated.ErrorModel
	if !errors.As(err, &problem) || problem.Status == nil {
		return err
	}
	var class error
	switch int(*problem.Status) {
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
