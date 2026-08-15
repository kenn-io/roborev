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

func browserMutationAllowsReview(job *storage.ReviewJob) bool {
	return job != nil &&
		job.IsReviewJob() &&
		!job.Agentic &&
		!job.PromptPrebuilt &&
		!job.UsesStoredPrompt()
}

// authorizeBrowserJobMutation restricts remote browser principals to ordinary
// code reviews. Stored-prompt and agentic jobs can run commands or modify the
// daemon owner's checkout, so only the loopback API and local browser sessions
// may cancel or rerun them.
func (s *Server) authorizeBrowserJobMutation(
	ctx context.Context, job *storage.ReviewJob,
) error {
	if !remoteBrowserPrincipal(ctx) {
		return nil
	}
	if browserMutationAllowsReview(job) {
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
			if !browserMutationAllowsReview(&members[i]) {
				return huma.Error403Forbidden(
					"remote browser sessions may only mutate non-agentic review jobs",
				)
			}
		}
		return nil
	}
	return huma.Error403Forbidden(
		"remote browser sessions may only mutate non-agentic review jobs",
	)
}
