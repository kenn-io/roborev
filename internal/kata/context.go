package kata

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Mode values for kata context resolution.
const (
	ModeOff     = "off"
	ModeCurrent = "current"
	ModeOpen    = "open"
)

// ContextResult is the outcome of resolving kata context for a review.
type ContextResult struct {
	Issues []Issue
	Notes  []string // human-facing notes (e.g. a referenced ref failed to load)
	Errs   []error  // resolution failures the caller should log; never fail the review
}

// ResolveContext loads kata issues for a review according to mode. It never
// fails the review for kata-specific problems: a missing binary or binding
// yields an empty result, and other failures are reported in Errs so a
// configured-but-broken setup is visible in logs instead of silently
// degrading. The single exception is context cancellation, which is returned
// as an error so a canceled review aborts instead of proceeding without
// context.
func ResolveContext(ctx context.Context, client Client, mode string, commitMessages []string) (ContextResult, error) {
	if client == nil {
		return ContextResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return ContextResult{}, err
	}
	binding, err := client.Binding(ctx)
	if err != nil {
		if canceled(err) {
			return ContextResult{}, err
		}
		if errors.Is(err, ErrNoBinding) || errors.Is(err, ErrUnavailable) {
			return ContextResult{}, nil // not a kata workspace -> inert
		}
		// A present-but-broken .kata.toml: surface it rather than silently
		// dropping the configured context.
		return ContextResult{Errs: []error{fmt.Errorf("resolve binding: %w", err)}}, nil
	}
	switch mode {
	case ModeOpen:
		issues, err := client.List(ctx, ListOpts{Status: "open"})
		if err != nil {
			if canceled(err) {
				return ContextResult{}, err
			}
			if errors.Is(err, ErrUnavailable) {
				return ContextResult{}, nil
			}
			return ContextResult{Errs: []error{fmt.Errorf("list open katas: %w", err)}}, nil
		}
		return ContextResult{Issues: excludeRoborevFiled(issues)}, nil
	case ModeCurrent:
		return resolveCurrent(ctx, client, binding.Project, commitMessages)
	default:
		return ContextResult{}, nil
	}
}

// canceled reports whether err stems from context cancellation or timeout.
func canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// excludeRoborevFiled drops issues filed by roborev itself (the review
// hook labels them RoborevLabel) so prior review findings are not replayed
// into new review prompts as authoritative task intent.
func excludeRoborevFiled(issues []Issue) []Issue {
	var kept []Issue
	for _, issue := range issues {
		if slices.Contains(issue.Labels, RoborevLabel) {
			continue
		}
		kept = append(kept, issue)
	}
	return kept
}

func resolveCurrent(ctx context.Context, client Client, project string, messages []string) (ContextResult, error) {
	var res ContextResult
	seen := make(map[string]bool)
	for _, msg := range messages {
		for _, ref := range ParseRefs(msg, project) {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			issue, err := client.Show(ctx, ref)
			if err != nil {
				if canceled(err) {
					return ContextResult{}, err
				}
				if errors.Is(err, ErrUnavailable) {
					return ContextResult{}, nil // kata absent -> inert, no notes
				}
				res.Notes = append(res.Notes, fmt.Sprintf("Referenced %s#%s could not be loaded.", project, ref))
				res.Errs = append(res.Errs, fmt.Errorf("show %s: %w", ref, err))
				continue
			}
			res.Issues = append(res.Issues, issue)
		}
	}
	return res, nil
}
