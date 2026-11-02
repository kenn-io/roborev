package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	googlegithub "github.com/google/go-github/v93/github"
)

// codeownersPaths are the locations GitHub reads a CODEOWNERS file from, in
// the order it checks them. GitHub uses the first file it finds.
var codeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// ParseCodeowners returns the user logins and "org/team" slugs that a
// CODEOWNERS file names as owners on any line. Email owners are ignored
// because they do not identify a GitHub login.
func ParseCodeowners(content string) (users, teams []string) {
	for line := range strings.Lines(content) {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, owner := range fields[1:] {
			if strings.HasPrefix(owner, "#") {
				break // the rest of the line is a comment
			}
			name, ok := strings.CutPrefix(owner, "@")
			if !ok || name == "" {
				continue
			}
			if strings.Contains(name, "/") {
				teams = append(teams, name)
			} else {
				users = append(users, name)
			}
		}
	}
	return users, teams
}

// CodeownerLogins returns the lowercase logins of every user that the
// repository's CODEOWNERS file at ref names as an owner, directly or through
// a team. A repository without a CODEOWNERS file has no owners.
func (c *Client) CodeownerLogins(ctx context.Context, ghRepo, ref string) (map[string]struct{}, error) {
	content, err := c.codeownersFile(ctx, ghRepo, ref)
	if err != nil {
		return nil, err
	}
	users, teams := ParseCodeowners(content)
	logins := make(map[string]struct{}, len(users))
	for _, user := range users {
		logins[strings.ToLower(user)] = struct{}{}
	}
	expanded := make(map[string]bool, len(teams))
	for _, team := range teams {
		// CODEOWNERS often names the same team on many lines.
		key := strings.ToLower(team)
		if expanded[key] {
			continue
		}
		expanded[key] = true
		members, err := c.listTeamMembers(ctx, team)
		if err != nil {
			return nil, fmt.Errorf("list members of CODEOWNERS team @%s: %w", team, err)
		}
		for _, member := range members {
			logins[strings.ToLower(member)] = struct{}{}
		}
	}
	return logins, nil
}

func (c *Client) codeownersFile(ctx context.Context, ghRepo, ref string) (string, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return "", err
	}
	opts := &googlegithub.RepositoryContentGetOptions{Ref: ref}
	for _, path := range codeownersPaths {
		file, _, err := readGitHub(ctx, "get CODEOWNERS", func() (*googlegithub.RepositoryContent, *googlegithub.Response, error) {
			file, _, resp, err := c.api.Repositories.GetContents(ctx, owner, repo, path, opts)
			return file, resp, err
		})
		if responseErr, ok := errors.AsType[*googlegithub.ErrorResponse](err); ok &&
			responseErr.Response != nil && responseErr.Response.StatusCode == http.StatusNotFound {
			continue
		}
		if err != nil {
			return "", err
		}
		if file == nil {
			continue // a directory, not a file
		}
		return file.GetContent()
	}
	return "", nil
}

func (c *Client) listTeamMembers(ctx context.Context, team string) ([]string, error) {
	org, slug, _ := strings.Cut(team, "/")
	opts := &googlegithub.TeamListTeamMembersOptions{PerPage: 100}
	var logins []string
	for {
		members, resp, err := readGitHub(ctx, "list team members", func() ([]*googlegithub.User, *googlegithub.Response, error) {
			return c.api.Teams.ListTeamMembersBySlug(ctx, org, slug, opts)
		})
		if err != nil {
			return nil, err
		}
		for _, member := range members {
			if login := member.GetLogin(); login != "" {
				logins = append(logins, login)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return logins, nil
		}
		opts.Page = resp.NextPage
	}
}
