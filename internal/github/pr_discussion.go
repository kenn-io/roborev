package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	googlegithub "github.com/google/go-github/v88/github"
)

const (
	PRDiscussionSourceIssueComment  = "issue_comment"
	PRDiscussionSourceReview        = "review"
	PRDiscussionSourceReviewComment = "review_comment"
)

type PRDiscussionComment struct {
	Author    string
	Body      string
	Source    string
	Path      string
	Line      int
	CreatedAt time.Time
}

// ListPRDiscussionComments returns human-authored pull request discussion
// comments across top-level issue comments, review summaries, and inline review
// comments. Results are sorted oldest-first.
func (c *Client) ListPRDiscussionComments(ctx context.Context, ghRepo string, prNumber int) ([]PRDiscussionComment, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return nil, err
	}

	var comments []PRDiscussionComment

	issueOpts := &googlegithub.IssueListCommentsOptions{
		Sort:      ptr("created"),
		Direction: ptr("asc"),
		ListOptions: googlegithub.ListOptions{
			PerPage: 100,
		},
	}
	for {
		issueComments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, prNumber, issueOpts)
		if err != nil {
			return nil, fmt.Errorf("list issue comments: %w", err)
		}
		for _, item := range issueComments {
			if !isHumanGitHubUser(item.User) || isRoborevCommentBody(item.GetBody()) || strings.TrimSpace(item.GetBody()) == "" {
				continue
			}
			comments = append(comments, PRDiscussionComment{
				Author:    item.GetUser().GetLogin(),
				Body:      item.GetBody(),
				Source:    PRDiscussionSourceIssueComment,
				CreatedAt: timestampValue(item.CreatedAt),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		issueOpts.Page = resp.NextPage
	}

	reviewOpts := &googlegithub.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := c.api.PullRequests.ListReviews(ctx, owner, repo, prNumber, reviewOpts)
		if err != nil {
			return nil, fmt.Errorf("list pull request reviews: %w", err)
		}
		for _, item := range reviews {
			if !isHumanGitHubUser(item.User) || isRoborevCommentBody(item.GetBody()) || strings.TrimSpace(item.GetBody()) == "" {
				continue
			}
			comments = append(comments, PRDiscussionComment{
				Author:    item.GetUser().GetLogin(),
				Body:      item.GetBody(),
				Source:    PRDiscussionSourceReview,
				CreatedAt: timestampValue(item.SubmittedAt),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}

	inlineOpts := &googlegithub.PullRequestListCommentsOptions{
		Sort:      "created",
		Direction: "asc",
		ListOptions: googlegithub.ListOptions{
			PerPage: 100,
		},
	}
	for {
		inlineComments, resp, err := c.api.PullRequests.ListComments(ctx, owner, repo, prNumber, inlineOpts)
		if err != nil {
			return nil, fmt.Errorf("list pull request comments: %w", err)
		}
		for _, item := range inlineComments {
			if !isHumanGitHubUser(item.User) || isRoborevCommentBody(item.GetBody()) || strings.TrimSpace(item.GetBody()) == "" {
				continue
			}
			comment := PRDiscussionComment{
				Author:    item.GetUser().GetLogin(),
				Body:      item.GetBody(),
				Source:    PRDiscussionSourceReviewComment,
				Path:      item.GetPath(),
				CreatedAt: timestampValue(item.CreatedAt),
			}
			if item.GetLine() > 0 {
				comment.Line = item.GetLine()
			} else if item.GetOriginalLine() > 0 {
				comment.Line = item.GetOriginalLine()
			}
			comments = append(comments, comment)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		inlineOpts.Page = resp.NextPage
	}

	sort.SliceStable(comments, func(i, j int) bool {
		return comments[i].CreatedAt.Before(comments[j].CreatedAt)
	})

	return comments, nil
}

// ListTrustedRepoCollaborators returns collaborator logins that have effective
// maintain or admin access to the repository. Logins are normalized to lower
// case for case-insensitive matching against GitHub comment authors.
func (c *Client) ListTrustedRepoCollaborators(ctx context.Context, ghRepo string) (map[string]struct{}, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return nil, err
	}

	opts := &googlegithub.ListCollaboratorsOptions{
		Affiliation: "all",
		ListOptions: googlegithub.ListOptions{PerPage: 100},
	}
	trusted := make(map[string]struct{})
	for {
		collaborators, resp, err := c.api.Repositories.ListCollaborators(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list collaborators: %w", err)
		}
		for _, item := range collaborators {
			login := strings.ToLower(strings.TrimSpace(item.GetLogin()))
			if login == "" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(item.GetRoleName())) {
			case "admin", "maintain":
				trusted[login] = struct{}{}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return trusted, nil
		}
		opts.Page = resp.NextPage
	}
}

func isHumanGitHubUser(user *googlegithub.User) bool {
	if user == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(user.GetType()), "bot") {
		return false
	}
	return !strings.HasSuffix(strings.TrimSpace(user.GetLogin()), "[bot]")
}

func isRoborevCommentBody(body string) bool {
	return strings.Contains(body, CommentMarker)
}

func timestampValue(ts *googlegithub.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.Time
}
