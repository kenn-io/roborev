package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestRepoResolver_Matching(t *testing.T) {
	// acmeRepos is a common set of repos returned by the mock API for "acme".
	acmeRepos := func(_ context.Context, owner string, _ string) ([]string, error) {
		if owner == "acme" {
			return []string{"acme/api", "acme/web", "acme/docs", "acme/api-gateway"}, nil
		}
		return nil, fmt.Errorf("unknown owner: %s", owner)
	}

	tests := []struct {
		name        string
		listReposFn func(context.Context, string, string) ([]string, error)
		canonicalFn func(context.Context, string, string) (string, error)
		ci          *config.CIConfig
		wantRepos   []string                   // expected repos (sorted); nil means don't check
		checkExtra  func(*testing.T, []string) // optional extra assertions
	}{
		{
			name: "exact only, no API calls",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				t.Error("listReposFn should not be called for exact-only config")
				return nil, fmt.Errorf("should not be called")
			},
			ci: &config.CIConfig{
				Repos: []string{"acme/api", "acme/web"},
			},
			wantRepos: []string{"acme/api", "acme/web"},
		},
		{
			name:        "wildcard expansion with prefix",
			listReposFn: acmeRepos,
			ci: &config.CIConfig{
				Repos: []string{"acme/api-*"},
			},
			// path.Match("acme/api-*", "acme/api") is false — only api-gateway matches
			wantRepos: []string{"acme/api-gateway"},
		},
		{
			name: "wildcard star matches all",
			listReposFn: func(_ context.Context, owner string, _ string) ([]string, error) {
				if owner == "myorg" {
					return []string{"myorg/api", "myorg/web", "myorg/docs"}, nil
				}
				return nil, fmt.Errorf("unknown owner: %s", owner)
			},
			ci: &config.CIConfig{
				Repos: []string{"myorg/*"},
			},
			wantRepos: []string{"myorg/api", "myorg/docs", "myorg/web"},
		},
		{
			name: "exclusion patterns",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				return []string{"acme/api", "acme/web", "acme/internal-tools", "acme/internal-docs", "acme/archived-v1"}, nil
			},
			ci: &config.CIConfig{
				Repos:        []string{"acme/*"},
				ExcludeRepos: []string{"acme/internal-*", "acme/archived-*"},
			},
			wantRepos: []string{"acme/api", "acme/web"},
		},
		{
			name: "exclusion applies to exact entries",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				t.Error("listReposFn should not be called for exact-only config")
				return nil, fmt.Errorf("should not be called")
			},
			ci: &config.CIConfig{
				Repos:        []string{"acme/api", "acme/internal-tools"},
				ExcludeRepos: []string{"acme/internal-*"},
			},
			wantRepos: []string{"acme/api"},
		},
		{
			name: "max repos cap",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				repos := make([]string, 200)
				for i := range repos {
					repos[i] = fmt.Sprintf("acme/repo-%03d", i)
				}
				return repos, nil
			},
			ci: &config.CIConfig{
				Repos:    []string{"acme/*"},
				MaxRepos: 5,
			},
			checkExtra: func(t *testing.T, repos []string) {
				t.Helper()
				assert.Len(t, repos, 5, "expected 5 repos (max_repos cap)")
			},
		},
		{
			name: "deduplication of exact and wildcard",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				return []string{"acme/api", "acme/web"}, nil
			},
			ci: &config.CIConfig{
				Repos: []string{"acme/api", "acme/*"},
			},
			checkExtra: func(t *testing.T, repos []string) {
				t.Helper()
				count := 0
				for _, r := range repos {
					if r == "acme/api" {
						count++
					}
				}
				assert.Equal(t, 1, count, "acme/api should appear exactly once in %v", repos)
			},
		},
		{
			name: "case insensitive wildcard matching",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				return []string{"Acme/API", "Acme/Web", "Acme/Docs"}, nil
			},
			ci: &config.CIConfig{
				Repos: []string{"acme/*"},
			},
			wantRepos: []string{"Acme/API", "Acme/Docs", "Acme/Web"},
		},
		{
			name: "case insensitive exclusion",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				return []string{"Acme/API", "Acme/Internal-Tools", "Acme/Web"}, nil
			},
			ci: &config.CIConfig{
				Repos:        []string{"acme/*"},
				ExcludeRepos: []string{"acme/internal-*"},
			},
			checkExtra: func(t *testing.T, repos []string) {
				t.Helper()
				assert.Len(t, repos, 2, "expected 2 repos after case-insensitive exclusion")
				for _, r := range repos {
					assert.False(t, strings.EqualFold(r, "Acme/Internal-Tools"),
						"excluded repo should not appear: %v", repos)
				}
			},
		},
		{
			name: "case insensitive dedup",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				return []string{"Acme/Api", "Acme/Web"}, nil
			},
			ci: &config.CIConfig{
				Repos: []string{"acme/api", "acme/*"},
			},
			checkExtra: func(t *testing.T, repos []string) {
				t.Helper()
				count := 0
				for _, r := range repos {
					if strings.EqualFold(r, "acme/api") {
						count++
					}
				}
				assert.Equal(t, 1, count,
					"api should appear once (case-insensitive dedup), got %d in %v", count, repos)
			},
		},
		{
			name: "moved exact repo deduped against wildcard canonical repo",
			listReposFn: func(_ context.Context, owner string, _ string) ([]string, error) {
				if owner == "kenn-io" {
					return []string{"kenn-io/kit", "kenn-io/roborev"}, nil
				}
				return nil, fmt.Errorf("unknown owner: %s", owner)
			},
			canonicalFn: func(_ context.Context, repo, _ string) (string, error) {
				if repo == "roborev-dev/roborev" {
					return "kenn-io/roborev", nil
				}
				return repo, nil
			},
			ci: &config.CIConfig{
				Repos: []string{"roborev-dev/roborev", "kenn-io/*"},
			},
			wantRepos: []string{"kenn-io/kit", "kenn-io/roborev"},
		},
		{
			name: "exclusion applies to original exact repo before canonicalization",
			canonicalFn: func(_ context.Context, repo, _ string) (string, error) {
				if repo == "old-org/repo" {
					return "new-org/repo", nil
				}
				return repo, nil
			},
			ci: &config.CIConfig{
				Repos:        []string{"old-org/repo"},
				ExcludeRepos: []string{"old-org/repo"},
			},
			wantRepos: []string{},
		},
		{
			name: "max repos preserves explicit entries",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				repos := make([]string, 20)
				for i := range repos {
					repos[i] = fmt.Sprintf("acme/aaa-%02d", i)
				}
				return repos, nil
			},
			ci: &config.CIConfig{
				Repos:    []string{"acme/zzz-important", "acme/*"},
				MaxRepos: 5,
			},
			checkExtra: func(t *testing.T, repos []string) {
				t.Helper()
				assert.Len(t, repos, 5, "expected 5 repos (max_repos cap)")
				assert.True(t, slices.Contains(repos, "acme/zzz-important"),
					"explicit repo acme/zzz-important was dropped by max_repos truncation: %v", repos)
			},
		},
		{
			name: "API failure falls back to exact entries",
			listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
				return nil, fmt.Errorf("network error")
			},
			ci: &config.CIConfig{
				Repos: []string{"acme/*", "acme/explicit-repo"},
			},
			wantRepos: []string{"acme/explicit-repo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RepoResolver{listReposFn: tt.listReposFn, canonicalRepoFn: tt.canonicalFn}

			repos, err := r.Resolve(context.Background(), tt.ci, nil)
			require.NoError(t, err)

			if tt.wantRepos != nil {
				assert.Equal(t, tt.wantRepos, repos)
			}
			if tt.checkExtra != nil {
				tt.checkExtra(t, repos)
			}
		})
	}
}

func TestRepoResolver_TokenFnCalled(t *testing.T) {
	var tokenOwners []string
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			return []string{"acme/api"}, nil
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*"},
	}

	tokenFn := func(owner string) string {
		tokenOwners = append(tokenOwners, owner)
		return "test-token"
	}

	_, err := r.Resolve(context.Background(), ci, tokenFn)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme"}, tokenOwners, "expected tokenFn called with [acme]")
}

func TestRepoResolver_CacheHit(t *testing.T) {
	var calls int
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			calls++
			return []string{"acme/api", "acme/web"}, nil
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*"},
	}

	ctx := context.Background()

	repos1, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "expected 1 API call")

	repos2, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "expected cache hit (still 1 call)")
	assert.Len(t, repos2, len(repos1), "cache returned different length")
}

func TestRepoResolver_CacheInvalidationOnConfigChange(t *testing.T) {
	var calls int
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			calls++
			return []string{"acme/api"}, nil
		},
	}

	ctx := context.Background()

	_, err := r.Resolve(ctx, &config.CIConfig{Repos: []string{"acme/*"}}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls)

	_, err = r.Resolve(ctx, &config.CIConfig{Repos: []string{"acme/*", "other/repo"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "expected cache miss on config change")
}

func TestRepoResolver_CacheInvalidationOnTTLExpiry(t *testing.T) {
	var calls int
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			calls++
			return []string{"acme/api"}, nil
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*"},
	}

	ctx := context.Background()
	_, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)

	_, err = r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "expected cache hit within TTL floor")

	// Force cache miss by backdating cachedAt past the TTL
	r.mu.Lock()
	r.cachedAt = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	_, err = r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "expected cache miss after TTL expiry")
}

func TestRepoResolver_CacheInvalidationOnMaxReposChange(t *testing.T) {
	var calls int
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			calls++
			repos := make([]string, 20)
			for i := range repos {
				repos[i] = fmt.Sprintf("acme/repo-%02d", i)
			}
			return repos, nil
		},
	}

	ctx := context.Background()

	repos1, err := r.Resolve(ctx, &config.CIConfig{Repos: []string{"acme/*"}, MaxRepos: 5}, nil)
	require.NoError(t, err)
	require.Len(t, repos1, 5)
	require.Equal(t, 1, calls)

	repos2, err := r.Resolve(ctx, &config.CIConfig{Repos: []string{"acme/*"}, MaxRepos: 10}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "expected cache miss on max_repos change")
	assert.Len(t, repos2, 10, "expected 10 repos after max_repos increase")
}

func TestRepoResolver_APIFailureFallback(t *testing.T) {
	var calls int
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			calls++
			return nil, fmt.Errorf("network error")
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*", "acme/explicit-repo"},
	}

	ctx := context.Background()

	repos, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/explicit-repo"}, repos, "expected only explicit repo on API failure")
	require.Equal(t, 1, calls)

	// Degraded results must NOT be cached — next call should retry the API
	repos2, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "expected degraded result to NOT be cached")
	assert.Equal(t, []string{"acme/explicit-repo"}, repos2)
}

func TestRepoResolver_EmptyResultsCached(t *testing.T) {
	var calls int
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			calls++
			return []string{}, nil
		},
	}

	ci := &config.CIConfig{
		Repos:        []string{"acme/nonexistent-*"},
		ExcludeRepos: []string{},
	}

	ctx := context.Background()

	repos1, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	require.Empty(t, repos1)
	require.Equal(t, 1, calls)

	repos2, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Empty(t, repos2)
	assert.Equal(t, 1, calls, "expected cache hit for empty result")
}

func TestRepoResolver_DegradedFallsBackToStaleCache(t *testing.T) {
	callCount := 0
	r := &RepoResolver{
		listReposFn: func(_ context.Context, _ string, _ string) ([]string, error) {
			callCount++
			if callCount == 1 {
				return []string{"acme/api", "acme/web"}, nil
			}
			return nil, fmt.Errorf("transient API error")
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*"},
	}
	ctx := context.Background()

	repos1, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	require.Len(t, repos1, 2)

	// Force TTL expiry so next call re-expands
	r.mu.Lock()
	r.cachedAt = time.Now().Add(-2 * time.Hour)
	r.mu.Unlock()

	// Second call fails API but should return stale cache
	repos2, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Len(t, repos2, 2, "expected stale cache (2 repos) on degraded")
}

func TestRepoResolver_CancelledContextReturnsError(t *testing.T) {
	r := &RepoResolver{
		listReposFn: func(ctx context.Context, _ string, _ string) ([]string, error) {
			return nil, ctx.Err()
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.Resolve(ctx, ci, nil)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRepoResolver_DeadlineExceededReturnsError(t *testing.T) {
	r := &RepoResolver{
		listReposFn: func(ctx context.Context, _ string, _ string) ([]string, error) {
			return nil, ctx.Err()
		},
	}

	ci := &config.CIConfig{
		Repos: []string{"acme/*"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	time.Sleep(time.Millisecond)

	_, err := r.Resolve(ctx, ci, nil)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestExactReposOnly(t *testing.T) {
	tests := []struct {
		name   string
		repos  []string
		expect []string
	}{
		{"all exact", []string{"acme/api", "acme/web"}, []string{"acme/api", "acme/web"}},
		{"mixed", []string{"acme/api", "acme/*", "other/repo"}, []string{"acme/api", "other/repo"}},
		{"all wildcards", []string{"acme/*", "other/api-*"}, nil},
		{"empty", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExactReposOnly(tt.repos)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestApplyExclusions(t *testing.T) {
	tests := []struct {
		name     string
		repos    []string
		patterns []string
		expect   []string
	}{
		{
			"no exclusions",
			[]string{"a/b", "c/d"},
			nil,
			[]string{"a/b", "c/d"},
		},
		{
			"exclude one pattern",
			[]string{"acme/api", "acme/internal-tools", "acme/web"},
			[]string{"acme/internal-*"},
			[]string{"acme/api", "acme/web"},
		},
		{
			"exclude multiple patterns",
			[]string{"acme/api", "acme/archived-v1", "acme/internal-docs"},
			[]string{"acme/archived-*", "acme/internal-*"},
			[]string{"acme/api"},
		},
		{
			"exclude all",
			[]string{"acme/internal-api"},
			[]string{"acme/internal-*"},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := make([]string, len(tt.repos))
			copy(input, tt.repos)
			got := applyExclusions(input, tt.patterns)
			if tt.expect == nil {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tt.expect, got)
			}
		})
	}
}
