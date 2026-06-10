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
// fails the review: a missing binary or binding yields an empty result, and
// other failures are reported in Errs so a configured-but-broken setup is
// visible in logs instead of silently degrading.
func ResolveContext(ctx context.Context, client Client, mode string, commitMessages []string) ContextResult {
	if client == nil {
		return ContextResult{}
	}
	binding, err := client.Binding(ctx)
	if err != nil {
		if errors.Is(err, ErrNoBinding) || errors.Is(err, ErrUnavailable) {
			return ContextResult{} // not a kata workspace -> inert
		}
		// A present-but-broken .kata.toml: surface it rather than silently
		// dropping the configured context.
		return ContextResult{Errs: []error{fmt.Errorf("resolve binding: %w", err)}}
	}
	switch mode {
	case ModeOpen:
		issues, err := client.List(ctx, ListOpts{Status: "open"})
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				return ContextResult{}
			}
			return ContextResult{Errs: []error{fmt.Errorf("list open katas: %w", err)}}
		}
		return ContextResult{Issues: excludeRoborevFiled(issues)}
	case ModeCurrent:
		return resolveCurrent(ctx, client, binding.Project, commitMessages)
	default:
		return ContextResult{}
	}
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

func resolveCurrent(ctx context.Context, client Client, project string, messages []string) ContextResult {
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
				if errors.Is(err, ErrUnavailable) {
					return ContextResult{} // kata absent -> inert, no notes
				}
				res.Notes = append(res.Notes, fmt.Sprintf("Referenced %s#%s could not be loaded.", project, ref))
				res.Errs = append(res.Errs, fmt.Errorf("show %s: %w", ref, err))
				continue
			}
			res.Issues = append(res.Issues, issue)
		}
	}
	return res
}
