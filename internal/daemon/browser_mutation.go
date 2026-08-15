package daemon

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

func remoteBrowserPrincipal(ctx context.Context) bool {
	principal, found := BrowserPrincipalFromContext(ctx)
	return found && !principal.Local
}

// eventForMutationPrincipal keeps remote browser mutations visible to live UI
// subscribers without allowing them to trigger privileged local hooks.
func eventForMutationPrincipal(ctx context.Context, event Event) Event {
	event.SuppressHooks = remoteBrowserPrincipal(ctx)
	return event
}

func browserCancellationAllowsReview(job *storage.ReviewJob) bool {
	return job != nil &&
		job.IsReviewJob() &&
		!job.Agentic &&
		!job.PromptPrebuilt &&
		!job.UsesStoredPrompt()
}

// authorizeBrowserJobCancellation restricts remote browser principals to
// ordinary code reviews. Remote reruns are rejected separately because even a
// nominally non-agentic agent can gain tools through daemon configuration.
func (s *Server) authorizeBrowserJobCancellation(
	ctx context.Context, job *storage.ReviewJob,
) error {
	if !remoteBrowserPrincipal(ctx) {
		return nil
	}
	if browserCancellationAllowsReview(job) {
		return nil
	}
	if job != nil && job.IsSynthesisJob() &&
		!job.Agentic && !job.PromptPrebuilt {
		members, err := s.db.GetPanelMembers(job.PanelRunUUID)
		if err != nil {
			return huma.Error500InternalServerError(
				fmt.Sprintf("authorize panel mutation: %v", err),
			)
		}
		for i := range members {
			if !browserCancellationAllowsReview(&members[i]) {
				return huma.Error403Forbidden(
					"remote browser sessions may only cancel non-agentic review jobs",
				)
			}
		}
		return nil
	}
	return huma.Error403Forbidden(
		"remote browser sessions may only cancel non-agentic review jobs",
	)
}
