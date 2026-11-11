package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	client "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

// A cache belongs to one command or refine iteration. The status check uses
// daemon configuration; the foreground client's configuration may be remote.
func newKataPromptCache() *prompt.KataContextCache {
	api := getDaemonEndpoint().APIClient(0)
	var checked, enabled bool
	var statusErr error
	return prompt.NewKataContextCache(func(ctx context.Context, review *storage.Review) (any, error) {
		if !checked {
			checked = true
			status, err := api.KataStatus(ctx, &generated.KataStatusRequestOptions{})
			statusErr = classifyKataPromptError(err)
			if status != nil {
				enabled = status.Enabled
			}
		}
		if statusErr != nil {
			return nil, statusErr
		}
		if !enabled {
			return nil, nil
		}
		id := review.UUID.String()
		query := &generated.KataPublicationQuery{JobID: &review.JobID, ReviewUUID: &id}
		result, err := api.KataPublication(ctx, &generated.KataPublicationRequestOptions{Query: query})
		if err != nil {
			return nil, classifyKataPromptError(err)
		}
		if result.State != "unavailable" || result.DocumentHash != "" {
			if err := prompt.ValidateKataContextSource(review, result.SourceDocument, result.DocumentHash); err != nil {
				return nil, err
			}
		}
		if result.State == "missing" {
			return nil, nil
		}
		return result, nil
	})
}

func classifyKataPromptError(err error) error {
	if err == nil {
		return nil
	}
	err = client.ClassifyKataContextError(err)
	if status, ok := daemonAnswerStatus(err); ok && (status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout) {
		return fmt.Errorf("%w: %w", prompt.ErrKataContextFatal, err)
	}
	if errors.Is(err, client.ErrKataAuthentication) || errors.Is(err, client.ErrKataAccess) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || daemon.IsDaemonAccessError(err) {
		return fmt.Errorf("%w: %w", prompt.ErrKataContextFatal, err)
	}
	return err
}
