package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"

	googlegithub "github.com/google/go-github/v88/github"
)

type OpenPullRequest struct {
	Number      int
	HeadRefOID  string
	BaseRefName string
	Title       string
	AuthorLogin string
}

type PullRequestInfo struct {
	Number      int
	State       string
	HeadRefOID  string
	BaseRefName string
	AuthorLogin string
}

func (c *Client) ListOpenPullRequests(ctx context.Context, ghRepo string, limit int) ([]OpenPullRequest, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}

	opts := &googlegithub.PullRequestListOptions{
		State: "open",
		ListOptions: googlegithub.ListOptions{
			PerPage: limit,
		},
	}

	prs, _, err := c.api.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("list pull requests: %w", err)
	}

	result := make([]OpenPullRequest, 0, len(prs))
	for _, pr := range prs {
		result = append(result, OpenPullRequest{
			Number:      pr.GetNumber(),
			HeadRefOID:  pr.GetHead().GetSHA(),
			BaseRefName: pr.GetBase().GetRef(),
			Title:       pr.GetTitle(),
			AuthorLogin: pr.GetUser().GetLogin(),
		})
	}
	return result, nil
}

func (c *Client) IsPullRequestOpen(ctx context.Context, ghRepo string, prNumber int) (bool, error) {
	pr, err := c.GetPullRequest(ctx, ghRepo, prNumber)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(pr.State, "open"), nil
}

func (c *Client) GetPullRequest(ctx context.Context, ghRepo string, prNumber int) (PullRequestInfo, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return PullRequestInfo{}, err
	}

	pr, _, err := c.api.PullRequests.Get(ctx, owner, repo, prNumber)
	if err != nil {
		return PullRequestInfo{}, fmt.Errorf("get pull request: %w", err)
	}
	return PullRequestInfo{
		Number:      pr.GetNumber(),
		State:       pr.GetState(),
		HeadRefOID:  pr.GetHead().GetSHA(),
		BaseRefName: pr.GetBase().GetRef(),
		AuthorLogin: pr.GetUser().GetLogin(),
	}, nil
}

func (c *Client) ListOwnerRepos(ctx context.Context, owner string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}

	repos, orgErr := c.listOrgRepos(ctx, owner, limit)
	if orgErr == nil {
		return repos, nil
	}
	if !isGitHubStatus(orgErr, 404) {
		return nil, orgErr
	}

	userRepos, userErr := c.listUserRepos(ctx, owner, limit)
	if userErr != nil {
		return nil, userErr
	}
	return userRepos, nil
}

func (c *Client) GetRepositoryFullName(ctx context.Context, ghRepo string) (string, error) {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return "", err
	}
	got, _, err := c.api.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("get repository: %w", err)
	}
	fullName := strings.TrimSpace(got.GetFullName())
	if fullName == "" {
		return ghRepo, nil
	}
	return fullName, nil
}

func (c *Client) SetCommitStatus(ctx context.Context, ghRepo, sha, state, description string) error {
	owner, repo, err := parseRepo(ghRepo)
	if err != nil {
		return err
	}

	_, _, err = c.api.Repositories.CreateStatus(ctx, owner, repo, sha, googlegithub.RepoStatus{
		State:       ptr(state),
		Description: ptr(description),
		Context:     ptr("roborev"),
	})
	if err != nil {
		return fmt.Errorf("create commit status: %w", err)
	}
	return nil
}

func CloneURL(ghRepo string) (string, error) {
	return CloneURLForBase(ghRepo, "")
}

func CloneURLForBase(ghRepo, rawBase string) (string, error) {
	if _, _, err := parseRepo(ghRepo); err != nil {
		return "", err
	}
	webBase, err := GitHubWebBaseURL(rawBase)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s.git", webBase, ghRepo), nil
}

func GitAuthEnv(baseEnv []string, token string) []string {
	return GitAuthEnvForBase(baseEnv, token, "")
}

func GitAuthEnvForBase(baseEnv []string, token, rawBase string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return baseEnv
	}

	webBase, err := GitHubWebBaseURL(rawBase)
	if err != nil {
		return baseEnv
	}

	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	env, configs := splitGitConfigEnv(baseEnv)
	configs = append(configs, gitConfigEntry{
		key:   "http." + webBase + ".extraheader",
		value: header,
	})

	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(configs)))
	for i, cfg := range configs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, cfg.key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, cfg.value),
		)
		if cfg.scope != "" {
			env = append(env, fmt.Sprintf("GIT_CONFIG_SCOPE_%d=%s", i, cfg.scope))
		}
	}
	return env
}

type gitConfigEntry struct {
	key   string
	value string
	scope string
}

func splitGitConfigEnv(baseEnv []string) ([]string, []gitConfigEntry) {
	env := make([]string, 0, len(baseEnv))
	keys := map[int]string{}
	values := map[int]string{}
	scopes := map[int]string{}
	configCount := 0
	hasConfigCount := false

	for _, entry := range baseEnv {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		switch {
		case name == "GIT_CONFIG_COUNT":
			parsed, err := strconv.Atoi(value)
			if err == nil && parsed >= 0 {
				configCount = parsed
				hasConfigCount = true
			}
		case strings.HasPrefix(name, "GIT_CONFIG_KEY_"):
			if index, err := strconv.Atoi(strings.TrimPrefix(name, "GIT_CONFIG_KEY_")); err == nil {
				keys[index] = value
			}
		case strings.HasPrefix(name, "GIT_CONFIG_VALUE_"):
			if index, err := strconv.Atoi(strings.TrimPrefix(name, "GIT_CONFIG_VALUE_")); err == nil {
				values[index] = value
			}
		case strings.HasPrefix(name, "GIT_CONFIG_SCOPE_"):
			if index, err := strconv.Atoi(strings.TrimPrefix(name, "GIT_CONFIG_SCOPE_")); err == nil {
				scopes[index] = value
			}
		default:
			env = append(env, entry)
		}
	}

	if !hasConfigCount {
		return env, nil
	}

	configs := make([]gitConfigEntry, 0, configCount)
	for i := 0; i < configCount; i++ {
		key, ok := keys[i]
		if !ok {
			continue
		}
		config := gitConfigEntry{
			key:   key,
			value: values[i],
			scope: scopes[i],
		}
		configs = append(configs, config)
	}
	return env, configs
}

func (c *Client) listOrgRepos(ctx context.Context, owner string, limit int) ([]string, error) {
	opts := &googlegithub.RepositoryListByOrgOptions{
		Type: "all",
		ListOptions: googlegithub.ListOptions{
			PerPage: min(limit, 100),
		},
	}
	return c.collectRepos(ctx, limit, func() ([]*googlegithub.Repository, *googlegithub.Response, error) {
		return c.api.Repositories.ListByOrg(ctx, owner, opts)
	}, func(nextPage int) {
		opts.Page = nextPage
	})
}

func (c *Client) listUserRepos(ctx context.Context, owner string, limit int) ([]string, error) {
	seen := make(map[string]struct{})
	var repos []string

	userOpts := &googlegithub.RepositoryListByUserOptions{
		Type: "owner",
		ListOptions: googlegithub.ListOptions{
			PerPage: min(limit, 100),
		},
	}
	pageRepos, err := c.collectRepos(ctx, limit, func() ([]*googlegithub.Repository, *googlegithub.Response, error) {
		return c.api.Repositories.ListByUser(ctx, owner, userOpts)
	}, func(nextPage int) {
		userOpts.Page = nextPage
	})
	if err != nil {
		return nil, err
	}
	for _, repo := range pageRepos {
		if _, ok := seen[strings.ToLower(repo)]; ok {
			continue
		}
		seen[strings.ToLower(repo)] = struct{}{}
		repos = append(repos, repo)
	}

	authOpts := &googlegithub.RepositoryListByAuthenticatedUserOptions{
		Affiliation: "owner,collaborator",
		Visibility:  "all",
		ListOptions: googlegithub.ListOptions{
			PerPage: min(limit, 100),
		},
	}
	for {
		authPage, resp, err := c.api.Repositories.ListByAuthenticatedUser(ctx, authOpts)
		if err != nil {
			break
		}
		for _, repo := range authPage {
			fullName := repo.GetFullName()
			if repo.GetArchived() || !strings.EqualFold(strings.TrimSpace(repoOwner(repo)), owner) {
				continue
			}
			if _, ok := seen[strings.ToLower(fullName)]; ok {
				continue
			}
			seen[strings.ToLower(fullName)] = struct{}{}
			repos = append(repos, fullName)
			if len(repos) >= limit {
				slices.Sort(repos)
				return repos[:limit], nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		authOpts.Page = resp.NextPage
	}

	slices.Sort(repos)
	if len(repos) > limit {
		repos = repos[:limit]
	}
	return repos, nil
}

func (c *Client) collectRepos(ctx context.Context, limit int, fetch func() ([]*googlegithub.Repository, *googlegithub.Response, error), setPage func(int)) ([]string, error) {
	var repos []string
	for {
		pageRepos, resp, err := fetch()
		if err != nil {
			return nil, fmt.Errorf("list repositories: %w", err)
		}
		for _, repo := range pageRepos {
			if repo.GetArchived() {
				continue
			}
			repos = append(repos, repo.GetFullName())
			if len(repos) >= limit {
				slices.Sort(repos)
				return repos, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			slices.Sort(repos)
			return repos, nil
		}
		setPage(resp.NextPage)
	}
}

func repoOwner(repo *googlegithub.Repository) string {
	if repo == nil || repo.Owner == nil {
		return ""
	}
	return repo.Owner.GetLogin()
}
