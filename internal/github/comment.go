package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	googlegithub "github.com/google/go-github/v88/github"

	"go.kenn.io/roborev/internal/review"
)

// CommentMarker is an invisible HTML marker embedded in every roborev PR
// comment so subsequent runs can find and update the existing comment
// instead of creating duplicates.
const CommentMarker = "<!-- roborev-pr-comment -->"

// FindExistingComment searches for an existing roborev comment on the
// given PR. It returns the comment ID if found, or 0 if no match exists.
func (c *Client) FindExistingComment(ctx context.Context, ghRepo string, prNumber int) (int64, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return 0, err
	}

	opts := &googlegithub.IssueListCommentsOptions{
		Sort:      ptr("created"),
		Direction: ptr("asc"),
		ListOptions: googlegithub.ListOptions{
			PerPage: 100,
		},
	}

	var lastID int64
	for {
		comments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, prNumber, opts)
		if err != nil {
			return 0, fmt.Errorf("list issue comments: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), CommentMarker) {
				lastID = comment.GetID()
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return lastID, nil
		}
		opts.Page = resp.NextPage
	}
}

// prepareBody prepends the CommentMarker and truncates to
// review.MaxCommentLen, preserving UTF-8 safety.
func prepareBody(body string) string {
	body = CommentMarker + "\n" + body

	const truncSuffix = "\n\n...(truncated — comment exceeded size limit)"
	maxBody := review.MaxCommentLen - len(truncSuffix)
	if len(body) > review.MaxCommentLen {
		body = review.TrimPartialRune(body[:maxBody]) + truncSuffix
	}
	return body
}

// CreatePRComment posts a new roborev PR comment. It prepends the
// CommentMarker and truncates to review.MaxCommentLen, then always
// creates a new comment (no find/patch).
func (c *Client) CreatePRComment(ctx context.Context, ghRepo string, prNumber int, body string) error {
	body = prepareBody(body)

	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return err
	}
	_, _, err = c.api.Issues.CreateComment(ctx, owner, repo, prNumber, &googlegithub.IssueComment{
		Body: ptr(body),
	})
	if err != nil {
		return fmt.Errorf("create PR comment: %w", err)
	}
	return nil
}

// UpsertPRComment creates or updates a roborev PR comment. It prepends
// the CommentMarker, truncates to review.MaxCommentLen, and either
// patches an existing comment or creates a new one.
func (c *Client) UpsertPRComment(ctx context.Context, ghRepo string, prNumber int, body string) error {
	body = prepareBody(body)

	existingID, err := c.FindExistingComment(ctx, ghRepo, prNumber)
	if err != nil {
		return fmt.Errorf("find existing comment: %w", err)
	}

	if existingID > 0 {
		if err := c.patchComment(ctx, ghRepo, existingID, body); err != nil {
			if isGitHubStatus(err, 403, 404) {
				log.Printf("warning: patch comment %d: %v (falling back to new comment)", existingID, err)
			} else {
				return fmt.Errorf("patch comment %d: %w", existingID, err)
			}
		} else {
			return nil
		}
	}
	return c.CreatePRComment(ctx, ghRepo, prNumber, body)
}

func (c *Client) patchComment(ctx context.Context, ghRepo string, commentID int64, body string) error {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return err
	}
	_, _, err = c.api.Issues.EditComment(ctx, owner, repo, commentID, &googlegithub.IssueComment{
		Body: ptr(body),
	})
	if err != nil {
		return fmt.Errorf("edit issue comment: %w", err)
	}
	return nil
}

func isGitHubStatus(err error, statuses ...int) bool {
	var githubErr *googlegithub.ErrorResponse
	if !errors.As(err, &githubErr) {
		return false
	}
	for _, status := range statuses {
		if githubErr.Response != nil && githubErr.Response.StatusCode == status {
			return true
		}
	}
	return false
}
