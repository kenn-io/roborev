package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	googlegithub "github.com/google/go-github/v90/github"

	"go.kenn.io/roborev/internal/review"
)

// CommentMarker is an invisible HTML marker embedded in every roborev PR
// comment so subsequent runs can find and update the existing comment
// instead of creating duplicates.
const CommentMarker = "<!-- roborev-pr-comment -->"

// FindExistingComment searches for an existing roborev comment on the
// given PR. It returns the comment ID if found, or 0 if no match exists.
func (c *Client) FindExistingComment(ctx context.Context, ghRepo string, prNumber int) (int64, error) {
	return c.findCommentPart(ctx, ghRepo, prNumber, CommentMarker)
}

func (c *Client) findCommentPart(ctx context.Context, ghRepo string, prNumber int, marker string) (int64, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return 0, err
	}

	opts := &googlegithub.IssueListCommentsOptions{
		Sort:      ptr("created"),
		Direction: ptr("asc"),
		PerPage:   100,
	}

	var lastID int64
	for {
		comments, resp, err := c.api.Issues.ListComments(ctx, owner, repo, prNumber, opts)
		if err != nil {
			return 0, fmt.Errorf("list issue comments: %w", err)
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), marker) {
				lastID = comment.GetID()
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return lastID, nil
		}
		opts.Page = resp.NextPage
	}
}

// prepareBodies measures provider formatting and preserves every input byte
// across as many comments as needed.
func prepareBodies(body string) []string {
	return review.CommentParts(body, func(body string, part int) string {
		return review.CommentPartMarker(CommentMarker, part) + "\n" + body
	})
}

// CreatePRComment posts the complete review as one or more new comments.
func (c *Client) CreatePRComment(ctx context.Context, ghRepo string, prNumber int, body string) error {
	for _, body := range prepareBodies(body) {
		if err := c.createPreparedComment(ctx, ghRepo, prNumber, body); err != nil {
			return err
		}
	}
	return nil
}

// UpsertPRComment updates the primary comment and its continuations. When
// a later review is shorter, surplus continuations no longer show stale findings.
func (c *Client) UpsertPRComment(ctx context.Context, ghRepo string, prNumber int, body string) error {
	parts := prepareBodies(body)
	for part, body := range parts {
		if err := c.upsertPreparedComment(ctx, ghRepo, prNumber, body, review.CommentPartMarker(CommentMarker, part)); err != nil {
			return err
		}
	}
	for part := len(parts); ; part++ {
		marker := review.CommentPartMarker(CommentMarker, part)
		id, err := c.findCommentPart(ctx, ghRepo, prNumber, marker)
		if err != nil {
			return err
		}
		if id == 0 {
			return nil
		}
		body := marker + "\nThis continuation has been superseded by the updated review."
		if err := c.patchComment(ctx, ghRepo, id, body); err != nil {
			return err
		}
	}
}

// createPreparedComment posts one already-sized part without preparing it again.
func (c *Client) createPreparedComment(ctx context.Context, ghRepo string, prNumber int, body string) error {
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

// upsertPreparedComment posts one already-sized part.
func (c *Client) upsertPreparedComment(ctx context.Context, ghRepo string, prNumber int, body, marker string) error {
	existingID, err := c.findCommentPart(ctx, ghRepo, prNumber, marker)
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
	return c.createPreparedComment(ctx, ghRepo, prNumber, body)
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
