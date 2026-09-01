package daemon

import (
	"context"

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
		!job.IsCIReview() &&
		job.PanelRole == "" &&
		job.PanelRunUUID == nil &&
		!job.Agentic &&
		!job.PromptPrebuilt &&
		!job.UsesStoredPrompt()
}

// authorizeBrowserJobCancellation restricts remote browser principals to
// standalone non-CI code reviews. Remote reruns are rejected separately
// because even a nominally non-agentic agent can gain tools through daemon
// configuration.
func (*Server) authorizeBrowserJobCancellation(
	ctx context.Context, job *storage.ReviewJob,
) error {
	if !remoteBrowserPrincipal(ctx) {
		return nil
	}
	if browserCancellationAllowsReview(job) {
		return nil
	}
	return huma.Error403Forbidden(
		"remote browser sessions may only cancel standalone non-CI reviews",
	)
}
