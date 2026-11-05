package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
	ghpkg "go.kenn.io/roborev/internal/github"
	"go.kenn.io/roborev/internal/review"
)

// repoRefreshInterval is the fixed interval between wildcard repo
// re-discovery calls to the GitHub API.
const repoRefreshInterval = time.Hour

// Retry failed discovery sooner than the normal hourly refresh.
const repoRetryInterval = 15 * time.Minute

type lookupUnit struct {
	val      []string
	err      error
	at       time.Time
	failures int
}

var errRepoDiscoveryIncomplete = errors.New("repository discovery incomplete")

// RepoResolver expands wildcard patterns in CI repo config into concrete
// "owner/repo" entries by querying the GitHub API. Results
// are cached for the refresh interval and automatically invalidated when
// the config changes.
type RepoResolver struct {
	mu       sync.Mutex
	units    map[string]*lookupUnit
	cacheKey string

	// baseURL is the raw GitHub API base URL for Enterprise support.
	// Empty means public GitHub. Used when building the API client
	// for wildcard repo expansion.
	baseURL string

	// listReposFn is a test seam. When non-nil it replaces the real
	// GitHub repo list call. Signature: (ctx, owner, token) -> []nameWithOwner.
	listReposFn func(ctx context.Context, owner, token string) ([]string, error)

	// canonicalRepoFn is a test seam / production hook for resolving exact repo
	// config entries through GitHub redirects, e.g. after a repository transfer.
	// Nil disables canonicalization, which keeps unit tests hermetic unless they
	// opt in.
	canonicalRepoFn func(ctx context.Context, ghRepo, token string) (string, error)
}

// githubTokenFn resolves an auth token for a given owner.
type githubTokenFn func(owner string) string

// Resolve returns the list of concrete "owner/repo" entries to poll.
// Successful lookups refresh hourly; failures retry with backoff.
// A discovery failure returns available repositories along with an error, so
// polling can continue without reporting the incomplete discovery as healthy.
func (r *RepoResolver) Resolve(ctx context.Context, ci *config.CIConfig, tokenFn githubTokenFn) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := r.buildCacheKey(ci)
	if r.units == nil || r.cacheKey != key {
		r.units = make(map[string]*lookupUnit)
		r.cacheKey = key
	}
	repos, degraded, err := r.expand(ctx, ci, tokenFn)
	if degraded {
		err = errors.Join(errRepoDiscoveryIncomplete, err)
	}
	return repos, err
}

// buildCacheKey invalidates all lookups when discovery config changes.
func (r *RepoResolver) buildCacheKey(ci *config.CIConfig) string {
	var installations strings.Builder
	for _, entry := range ci.Repos {
		owner, _, _ := strings.Cut(entry, "/")
		fmt.Fprintf(&installations, "%d\x00", ci.InstallationIDForOwner(owner))
	}
	return strings.Join(ci.Repos, "\x00") + "\x01" +
		strings.Join(ci.ExcludeRepos, "\x00") + "\x02" +
		fmt.Sprintf("%d", ci.ResolvedMaxRepos()) + "\x03" + installations.String()
}

func repoLookupKey(kind, name string) string {
	return kind + ":" + strings.ToLower(name)
}

// lookup keeps the last successful value available while a refresh fails.
// The caller holds mu. Token resolution happens only when a lookup is due.
func (r *RepoResolver) lookup(ctx context.Context, key, owner string, tokenFn githubTokenFn, fetch func(context.Context, string) ([]string, error)) ([]string, error) {
	unit := r.units[key]
	if unit != nil {
		ttl := repoRefreshInterval
		if unit.err != nil {
			ttl = (review.RetrySchedule{Base: repoRetryInterval, Cap: repoRefreshInterval}).NextDelay(unit.failures)
		}
		if time.Since(unit.at) < ttl {
			return unit.val, unit.err
		}
	}
	var token string
	if tokenFn != nil {
		token = tokenFn(owner)
	}
	val, err := fetch(ctx, token)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if unit == nil {
		unit = &lookupUnit{}
		r.units[key] = unit
	}
	unit.at, unit.err = time.Now(), err
	if err == nil {
		unit.val, unit.failures = val, 0
	} else {
		unit.failures++
	}
	return unit.val, err
}

// expand separates exact entries from wildcard patterns, calls the
// GitHub API once per owner for wildcards, applies matching and
// exclusions, deduplicates, and enforces max_repos.
//
// The returned degraded flag is true when one or more GitHub API calls failed
// during exact-repo canonicalization or wildcard expansion, including cached errors.
func (r *RepoResolver) expand(ctx context.Context, ci *config.CIConfig, tokenFn githubTokenFn) ([]string, bool, error) {
	var exact []string
	var degraded bool
	var discoveryErrors []error
	// owner → list of full patterns like "owner/pattern"
	wildcardsByOwner := make(map[string][]string)
	exclusionPatterns := validExclusionPatterns(ci.ExcludeRepos)

	for _, entry := range ci.Repos {
		owner, repoPattern, ok := strings.Cut(entry, "/")
		if !ok || owner == "" {
			log.Printf("CI repo resolver: skipping invalid entry %q (expected owner/repo)", entry)
			continue
		}
		if isGlobPattern(repoPattern) {
			wildcardsByOwner[owner] = append(wildcardsByOwner[owner], entry)
		} else {
			exact = append(exact, entry)
		}
	}

	// Deduplicate exact entries. Canonicalize them first when the poller has
	// wired a GitHub lookup, so an old moved name like "old/repo" can collapse
	// into the same key as a wildcard-expanded "new/repo".
	seen := make(map[string]bool, len(exact))
	var exactResult []string
	for _, e := range exact {
		if repoMatchesExclusionPatterns(e, exclusionPatterns) {
			continue
		}
		canonical := e
		if r.canonicalRepoFn != nil {
			owner, _, _ := strings.Cut(e, "/")
			values, canonicalErr := r.lookup(ctx, repoLookupKey("exact", e), owner, tokenFn, func(ctx context.Context, token string) ([]string, error) {
				canonical, err := r.canonicalizeExactRepo(ctx, e, token)
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("CI repo resolver: failed to canonicalize repo %q: %v (using last known or configured name)", e, err)
					}
					return nil, err
				}
				return []string{canonical}, nil
			})
			if len(values) > 0 {
				canonical = values[0]
			}
			if canonicalErr != nil {
				if ctx.Err() != nil {
					return nil, false, ctx.Err()
				}
				degraded = true
				discoveryErrors = append(discoveryErrors, canonicalErr)
			}
		}
		lower := strings.ToLower(canonical)
		if !seen[lower] {
			seen[lower] = true
			exactResult = append(exactResult, canonical)
		}
	}

	// Expand wildcards by owner
	var wildcardResult []string
	for owner, patterns := range wildcardsByOwner {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}

		repos, err := r.lookup(ctx, repoLookupKey("owner", owner), owner, tokenFn, func(ctx context.Context, token string) ([]string, error) {
			repos, err := r.callListRepos(ctx, owner, token)
			if err != nil && ctx.Err() == nil {
				log.Printf("CI repo resolver: failed to list repos for %q: %v (using last known repos)", owner, err)
			}
			return repos, err
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			degraded = true
			discoveryErrors = append(discoveryErrors, err)
		}

		for _, repo := range repos {
			repoLower := strings.ToLower(repo)
			for _, pattern := range patterns {
				matched, matchErr := path.Match(
					strings.ToLower(pattern), repoLower,
				)
				if matchErr != nil {
					log.Printf("CI repo resolver: invalid pattern %q: %v", pattern, matchErr)
					continue
				}
				if matched {
					if !seen[repoLower] {
						seen[repoLower] = true
						wildcardResult = append(wildcardResult, repo)
					}
					break // no need to match other patterns for same repo
				}
			}
		}
	}

	// Apply exclusions to both sets
	exactResult = applyExclusionsWithPatterns(exactResult, exclusionPatterns)
	wildcardResult = applyExclusionsWithPatterns(wildcardResult, exclusionPatterns)

	// Enforce max_repos: explicit repos have priority over wildcard-expanded repos.
	maxRepos := ci.ResolvedMaxRepos()
	total := len(exactResult) + len(wildcardResult)

	if total > maxRepos {
		// Sort wildcards for deterministic truncation
		sort.Strings(wildcardResult)

		remaining := maxRepos - len(exactResult)
		if remaining <= 0 {
			// Even exact repos exceed limit — truncate exact too
			sort.Strings(exactResult)
			exactResult = exactResult[:maxRepos]
			wildcardResult = nil
		} else if len(wildcardResult) > remaining {
			wildcardResult = wildcardResult[:remaining]
		}

		log.Printf("CI repo resolver: expanded to %d repos, truncating to max_repos=%d (keeping %d explicit)", total, maxRepos, len(exactResult))
	}

	// Combine and sort for deterministic output
	result := make([]string, 0, len(exactResult)+len(wildcardResult))
	result = append(result, exactResult...)
	result = append(result, wildcardResult...)
	sort.Strings(result)

	return result, degraded, errors.Join(discoveryErrors...)
}

// callListRepos invokes the GitHub client or the test seam to list repos for an owner.
func (r *RepoResolver) callListRepos(ctx context.Context, owner, token string) ([]string, error) {
	if r.listReposFn != nil {
		return r.listReposFn(ctx, owner, token)
	}
	return ghListRepos(ctx, owner, token, r.baseURL)
}

func (r *RepoResolver) canonicalizeExactRepo(ctx context.Context, ghRepo, token string) (string, error) {
	canonical, err := r.canonicalRepoFn(ctx, ghRepo, token)
	if err != nil {
		return "", err
	}
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return ghRepo, nil
	}
	return canonical, nil
}

func ghListRepos(ctx context.Context, owner, token, rawBaseURL string) ([]string, error) {
	apiBaseURL, err := ghpkg.GitHubAPIBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	client, err := ghpkg.NewClient(token, ghpkg.WithBaseURL(apiBaseURL))
	if err != nil {
		return nil, err
	}
	return client.ListOwnerRepos(ctx, owner, 1000)
}

func ghCanonicalRepo(ctx context.Context, ghRepo, token, rawBaseURL string) (string, error) {
	apiBaseURL, err := ghpkg.GitHubAPIBaseURL(rawBaseURL)
	if err != nil {
		return "", err
	}
	client, err := ghpkg.NewClient(token, ghpkg.WithBaseURL(apiBaseURL))
	if err != nil {
		return "", err
	}
	return client.GetRepositoryFullName(ctx, ghRepo)
}

// applyExclusions filters repos matching any of the exclusion patterns.
// Matching is case-insensitive. Invalid patterns are logged once and skipped.
func applyExclusions(repos []string, patterns []string) []string {
	return applyExclusionsWithPatterns(repos, validExclusionPatterns(patterns))
}

func validExclusionPatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}

	// Pre-validate and lowercase patterns once to avoid repeated
	// error logging inside the inner loop.
	valid := make([]string, 0, len(patterns))
	for _, p := range patterns {
		lower := strings.ToLower(p)
		if _, err := path.Match(lower, ""); err != nil {
			log.Printf("CI repo resolver: invalid exclusion pattern %q: %v", p, err)
			continue
		}
		valid = append(valid, lower)
	}
	return valid
}

func applyExclusionsWithPatterns(repos []string, valid []string) []string {
	if len(valid) == 0 {
		return repos
	}

	filtered := repos[:0]
	for _, repo := range repos {
		if !repoMatchesExclusionPatterns(repo, valid) {
			filtered = append(filtered, repo)
		}
	}
	return filtered
}

func repoMatchesExclusionPatterns(repo string, valid []string) bool {
	repoLower := strings.ToLower(repo)
	for _, pattern := range valid {
		if matched, _ := path.Match(pattern, repoLower); matched {
			return true
		}
	}
	return false
}

// isGlobPattern returns true if the string contains glob metacharacters.
func isGlobPattern(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// ExactReposOnly returns the subset of ci.Repos that are exact entries
// (no glob characters). Used as a fallback when resolver expansion fails.
func ExactReposOnly(repos []string) []string {
	var exact []string
	for _, entry := range repos {
		owner, repoPattern, ok := strings.Cut(entry, "/")
		if ok && owner != "" && !isGlobPattern(repoPattern) {
			exact = append(exact, entry)
		}
	}
	return exact
}
