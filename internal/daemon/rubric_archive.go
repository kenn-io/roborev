package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/storage"
)

// These operator privacy and best-effort policy limits are not provider maxima.
const (
	rubricArchiveTimeout   = 30 * time.Second
	rubricArchiveMaxBytes  = 1 << 20
	rubricArchiveMaxIssues = 10
)

func archiveRubricMessages(ctx context.Context, client kata.Client, project string, messages []string, name string, data []byte, report func(error)) {
	seen := map[string]bool{}
	for _, msg := range messages {
		for _, ref := range kata.ParseRefs(msg, project) {
			if ctx.Err() != nil {
				return
			}
			issue, err := client.ResolveArchiveIssue(ctx, project, ref)
			if err != nil {
				report(fmt.Errorf("resolve rubric destination %s#%s: %w", project, ref, err))
				if archiveGlobalFailure(ctx, err) {
					return
				}
				continue
			}
			if seen[issue.UID] {
				continue
			}
			if len(seen) == rubricArchiveMaxIssues {
				return
			}
			seen[issue.UID] = true
			if err := client.ArchiveRubric(ctx, project, issue.UID, name, data); err != nil {
				report(fmt.Errorf("attach rubric to %s#%s: %w", project, ref, err))
				if archiveGlobalFailure(ctx, err) {
					return
				}
			}
		}
	}
}

func archiveGlobalFailure(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, kata.ErrUnavailable) || errors.Is(err, kata.ErrDaemonUnavailable) || errors.Is(err, kata.ErrDocbankNotConfigured)
}

func archiveRubric(ctx context.Context, client kata.Client, job storage.ReviewJob, repoPath string, data []byte, digest string, report func(error)) {
	binding, err := client.Binding(ctx)
	if err != nil {
		report(err)
		return
	}
	commits := []string{job.GitRef}
	if strings.Contains(job.GitRef, "..") {
		commits, err = gitpkg.GetRangeCommitsCtx(ctx, repoPath, job.GitRef)
		if err != nil {
			report(fmt.Errorf("read reviewed commits: %w", err))
			return
		}
	}
	var messages []string
	for _, ref := range commits {
		info, err := gitpkg.GetCommitInfoCtx(ctx, repoPath, ref)
		if err != nil {
			report(fmt.Errorf("read reviewed commit: %w", err))
			return
		}
		messages = append(messages, info.Subject+"\n\n"+info.Body)
	}
	name := fmt.Sprintf("roborev-rubric.%s.%s.md", job.ReviewType, digest[:16])
	archiveRubricMessages(ctx, client, binding.Project, messages, name, data, report)
}

func prepareRubricArchive(job *storage.ReviewJob, enabled bool, data []byte) bool {
	if !enabled || job.IsCIReview() || job.Source == "ci_branch" || job.IsDirtyJob() || !job.IsReviewJob() || job.IsGoalReviewJob() || job.PromptPrebuilt || job.Source == storage.JobSourceReviewPrompt {
		return false
	}
	if len(data) > rubricArchiveMaxBytes {
		log.Printf("rubric archive for job %d skipped: rubric exceeds 1 MiB", job.ID)
		return false
	}
	return true
}
