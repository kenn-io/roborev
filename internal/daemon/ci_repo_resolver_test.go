package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
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
		wantErr     error
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
			name:    "API failure falls back to exact entries",
			wantErr: errRepoDiscoveryIncomplete,
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

			repos, err := r.Resolve(t.Context(), tt.ci, nil)
			require.ErrorIs(t, err, tt.wantErr)

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

	_, err := r.Resolve(t.Context(), ci, tokenFn)
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

	ctx := t.Context()

	repos1, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	require.Equal(t, 1, calls, "expected 1 API call")

	repos2, err := r.Resolve(ctx, ci, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "expected cache hit (still 1 call)")
	assert.Len(t, repos2, len(repos1), "cache returned different length")
}

func TestRepoResolver_CacheInvalidationOnConfigChange(t *testing.T) {
	calls := make(map[string]int)
	r := &RepoResolver{canonicalRepoFn: func(_ context.Context, repo, _ string) (string, error) {
		calls[repo]++
		return repo, nil
	}}
	ci := &config.CIConfig{Repos: []string{"acme/api", "acme/old"}}
	repos, err := r.Resolve(t.Context(), ci, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/api", "acme/old"}, repos)
	ci.Repos = []string{"acme/api", "acme/new"}
	repos, err = r.Resolve(t.Context(), ci, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/api", "acme/new"}, repos)
	assert.Equal(t, map[string]int{"acme/api": 1, "acme/old": 1, "acme/new": 1}, calls)
	ci.Repos = []string{"acme/old"}
	repos, err = r.Resolve(t.Context(), ci, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/old"}, repos)
	assert.Equal(t, 2, calls["acme/old"], "removed entries lose their cached result")
}

func TestRepoResolver_CacheInvalidationOnTTLExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		ctx := t.Context()
		_, err := r.Resolve(ctx, ci, nil)
		require.NoError(t, err)

		_, err = r.Resolve(ctx, ci, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, calls, "expected cache hit within TTL floor")

		time.Sleep(time.Hour)

		_, err = r.Resolve(ctx, ci, nil)
		require.NoError(t, err)
		assert.Equal(t, 2, calls, "expected cache miss after TTL expiry")
	})
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

	ctx := t.Context()

	repos1, err := r.Resolve(ctx, &config.CIConfig{Repos: []string{"acme/*"}, MaxRepos: 5}, nil)
	require.NoError(t, err)
	require.Len(t, repos1, 5)
	require.Equal(t, 1, calls)

	repos2, err := r.Resolve(ctx, &config.CIConfig{Repos: []string{"acme/*"}, MaxRepos: 10}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "max_repos reapplies to cached discovery")
	assert.Len(t, repos2, 10, "expected 10 repos after max_repos increase")
}

func TestRepoResolver_APIFailureFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		calls := make(map[string]int)
		tokens := 0
		failing := true
		r := &RepoResolver{
			canonicalRepoFn: func(_ context.Context, repo, token string) (string, error) {
				assert.Equal("synthetic-token", token)
				calls[repo]++
				if repo == "other/missing" && failing {
					return "", fmt.Errorf("repository unavailable")
				}
				if repo == "other/missing" {
					return "other/recovered", nil
				}
				return repo, nil
			},
			listReposFn: func(_ context.Context, owner, token string) ([]string, error) {
				assert.Equal("acme", owner)
				assert.Equal("synthetic-token", token)
				calls[owner]++
				return []string{"acme/web"}, nil
			},
		}
		tokenFn := func(string) string { tokens++; return "synthetic-token" }
		ci := &config.CIConfig{Repos: []string{"acme/api", "acme/docs", "acme/tools", "other/missing", "acme/*"}}
		want := []string{"acme/api", "acme/docs", "acme/tools", "acme/web", "other/missing"}
		repos, err := r.Resolve(t.Context(), ci, tokenFn)
		require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
		assert.Equal(want, repos)
		assert.Equal(5, tokens)
		start := time.Now()
		for _, step := range []struct {
			minute       int
			failedCalls  int
			healthyCalls int
		}{
			{5, 1, 1}, {14, 1, 1}, {15, 2, 1},
			{20, 2, 1}, {44, 2, 1}, {45, 3, 1},
			{59, 3, 1}, {60, 3, 2}, {104, 3, 2}, {105, 4, 2},
			{119, 4, 2}, {120, 4, 3}, {164, 4, 3}, {165, 5, 3},
		} {
			time.Sleep(time.Duration(step.minute)*time.Minute - time.Since(start))
			repos, err = r.Resolve(t.Context(), ci, tokenFn)
			require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
			assert.Equal(want, repos)
			assert.Equal(map[string]int{"acme/api": step.healthyCalls, "acme/docs": step.healthyCalls, "acme/tools": step.healthyCalls, "other/missing": step.failedCalls, "acme": step.healthyCalls}, calls)
			assert.Equal(step.healthyCalls*4+step.failedCalls, tokens, "token resolution only accompanies discovery calls")
		}
		failing = false
		time.Sleep(time.Hour)
		repos, err = r.Resolve(t.Context(), ci, tokenFn)
		require.NoError(t, err)
		assert.Equal([]string{"acme/api", "acme/docs", "acme/tools", "acme/web", "other/recovered"}, repos)
		failing = true
		time.Sleep(time.Hour)
		repos, err = r.Resolve(t.Context(), ci, tokenFn)
		require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
		assert.Equal([]string{"acme/api", "acme/docs", "acme/tools", "acme/web", "other/recovered"}, repos)
		before := calls["other/missing"]
		time.Sleep(15 * time.Minute)
		_, err = r.Resolve(t.Context(), ci, tokenFn)
		require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
		assert.Equal(before+1, calls["other/missing"], "recovery resets backoff")
	})
}

func TestRepoResolver_InstallationChange(t *testing.T) {
	calls := make(map[string]int)
	r := &RepoResolver{
		canonicalRepoFn: func(_ context.Context, repo, _ string) (string, error) {
			calls[repo]++
			return "", fmt.Errorf("repository unavailable")
		},
		listReposFn: func(_ context.Context, owner, _ string) ([]string, error) {
			calls[owner]++
			return nil, fmt.Errorf("owner unavailable")
		},
	}
	ci := &config.CIConfig{Repos: []string{"acme/api", "acme/*", "other/*"}}
	_, err := r.Resolve(t.Context(), ci, nil)
	require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
	ci.GitHubAppInstallations = map[string]int64{"acme": 42}
	_, err = r.Resolve(t.Context(), ci, nil)
	require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
	assert.Equal(t, map[string]int{"acme/api": 2, "acme": 2, "other": 1}, calls)
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

	ctx := t.Context()

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
	synctest.Test(t, func(t *testing.T) {
		calls := make(map[string]int)
		r := &RepoResolver{listReposFn: func(_ context.Context, owner, _ string) ([]string, error) {
			calls[owner]++
			if owner == "acme" {
				if calls[owner] > 1 {
					return nil, fmt.Errorf("owner unavailable")
				}
				return []string{"acme/api", "acme/web"}, nil
			}
			if calls[owner] == 1 {
				return []string{"other/old"}, nil
			}
			return []string{"other/new"}, nil
		}}
		ci := &config.CIConfig{Repos: []string{"acme/*", "other/*"}}
		repos, err := r.Resolve(t.Context(), ci, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"acme/api", "acme/web", "other/old"}, repos)
		time.Sleep(time.Hour)
		repos, err = r.Resolve(t.Context(), ci, nil)
		require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
		assert.Equal(t, []string{"acme/api", "acme/web", "other/new"}, repos)
		time.Sleep(15 * time.Minute)
		repos, err = r.Resolve(t.Context(), ci, nil)
		require.ErrorIs(t, err, errRepoDiscoveryIncomplete)
		assert.Equal(t, []string{"acme/api", "acme/web", "other/new"}, repos)
		assert.Equal(t, map[string]int{"acme": 3, "other": 2}, calls)
	})
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

	ctx, cancel := context.WithCancel(t.Context())
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

	ctx, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()

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
