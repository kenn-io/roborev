package daemon

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// A supplied review prompt keeps commit/range semantics. Reject inputs that
// would reinterpret it as a task, fan it out, or derive other review work.
func validateReviewPromptRequest(req EnqueueRequest) error {
	if strings.TrimSpace(req.ReviewPrompt) == "" {
		return fmt.Errorf("review_prompt must contain review instructions")
	}
	ref := req.GitRef
	if ref == "" {
		ref = req.CommitSHA
	}
	if ref == "dirty" || req.DiffContent != "" || len(req.DirtyFiles) != 0 ||
		req.CustomPrompt != "" || req.Agentic || req.JobType != "" ||
		(req.Panel != "" && req.Panel != "none") || req.Source != "" ||
		req.SpecFile != nil || req.PlanFile != nil || req.ReviewType == config.ReviewTypeGoal ||
		req.Since != "" || req.AnalysisType != "" || len(req.AnalysisFiles) != 0 || req.AnalysisCommitSHA != "" {
		return fmt.Errorf("review_prompt requires a single commit or range and cannot be combined with task, dirty, panel, automation, or agentic inputs")
	}
	return nil
}
