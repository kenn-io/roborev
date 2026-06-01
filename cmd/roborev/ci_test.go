package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func installFakeGHAuthToken(t *testing.T, token string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping fake gh helper on Windows")
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nif [ \"$1\" = \"auth\" ] && [ \"$2\" = \"token\" ]; then\n  printf '%s\\n' " + "'" + token + "'\n  exit 0\nfi\nexit 1\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCIReviewCmd_Help(t *testing.T) {
	cmd := ciCmd()
	cmd.SetArgs([]string{"review", "--help"})

	// Capture output
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	err := cmd.Execute()
	require.NoError(t, err)

	output := buf.String()
	checks := []string{
		"--ref",
		"--comment",
		"--gh-repo",
		"--pr",
		"--agent",
		"--review-types",
		"--reasoning",
		"--min-severity",
		"--synthesis-agent",
	}
	for _, check := range checks {
		assert.Contains(t, output, check)
	}
}

func TestCIReviewCmd_Validation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	tests := []struct {
		name      string
		args      []string
		wantError string
		clearEnv  bool
	}{
		{"InvalidReviewType", []string{"review", "--ref", "abc", "--review-types", "bogus"}, "invalid review_type", false},
		{"InvalidReasoning", []string{"review", "--ref", "abc", "--reasoning", "bogus"}, "invalid reasoning", false},
		{"InvalidMinSeverity", []string{"review", "--ref", "abc", "--min-severity", "bogus"}, "invalid min_severity", false},
		{"RequiresRef", []string{"review"}, "auto-detection", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.clearEnv {
				t.Setenv("GITHUB_EVENT_PATH", "")
				t.Setenv("GITHUB_REF", "")
			}
			cmd := ciCmd()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()

			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func setupFakeGitHubEvent(t *testing.T, event map[string]any) {
	t.Helper()
	eventFile := filepath.Join(t.TempDir(), "event.json")
	data, _ := json.Marshal(event)
	if err := os.WriteFile(eventFile, data, 0o644); err != nil {
		require.NoError(t, err)
	}
	t.Setenv("GITHUB_EVENT_PATH", eventFile)
}

func TestDetectGitRef(t *testing.T) {
	setupFakeGitHubEvent(t, map[string]any{
		"pull_request": map[string]any{
			"base": map[string]string{
				"sha": "aaa111",
			},
			"head": map[string]string{
				"sha": "bbb222",
			},
		},
	})

	ref, err := detectGitRef()
	require.NoError(t, err)

	assert.Equal(t, "aaa111..bbb222", ref)
}

func TestDetectGitRef_NoEnv(t *testing.T) {
	t.Setenv("GITHUB_EVENT_PATH", "")

	_, err := detectGitRef()
	require.Error(t, err)
}

func TestDetectPRNumber_EventJSON(t *testing.T) {
	setupFakeGitHubEvent(t, map[string]any{
		"pull_request": map[string]any{
			"number": 42,
		},
	})

	pr, err := detectPRNumber()
	require.NoError(t, err)

	assert.Equal(t, 42, pr)
}

func TestDetectPRNumber_GitHubRef(t *testing.T) {
	t.Setenv("GITHUB_EVENT_PATH", "")
	t.Setenv("GITHUB_REF", "refs/pull/123/merge")

	pr, err := detectPRNumber()
	require.NoError(t, err)

	assert.Equal(t, 123, pr)
}

func TestDetectPRNumber_NoEnv(t *testing.T) {
	t.Setenv("GITHUB_EVENT_PATH", "")
	t.Setenv("GITHUB_REF", "")

	_, err := detectPRNumber()
	require.Error(t, err)
}

func TestExtractHeadSHA(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"aaa..bbb", "bbb"},
		{"abc123", "abc123"},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractHeadSHA(tt.ref)
		assert.Equal(t, tt.want, got)
	}
}

func TestResolveAgentList(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		agents := config.ResolveCIAgents(
			"codex,gemini", nil, nil)
		assert.False(t, len(agents) != 2 ||
			agents[0] != "codex" ||
			agents[1] != "gemini")
	})

	t.Run("default", func(t *testing.T) {
		agents := config.ResolveCIAgents("", nil, nil)
		assert.False(t, len(agents) != 1 || agents[0] != "")
	})
}

func TestResolveReviewTypes(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		types := config.ResolveCIReviewTypes(
			"security,design", nil, nil)
		assert.Len(t, types, 2)
	})

	t.Run("default", func(t *testing.T) {
		types := config.ResolveCIReviewTypes("", nil, nil)
		assert.False(t, len(types) != 1 || types[0] != "security")
	})
}

func TestResolveAgentList_EmptyFlag(t *testing.T) {
	// Comma-only flag should resolve to empty list.
	agents := config.ResolveCIAgents(",", nil, nil)
	assert.Empty(t, agents)
}

func TestResolveReviewTypes_EmptyFlag(t *testing.T) {
	// Whitespace-comma flag should resolve to empty list.
	types := config.ResolveCIReviewTypes(" , ", nil, nil)
	assert.Empty(t, types)
}

func TestResolveCIUpsertComments(t *testing.T) {
	tests := []struct {
		name   string
		repo   *config.RepoConfig
		global *config.Config
		want   bool
	}{
		{
			name: "nil/nil defaults to false",
			repo: nil, global: nil, want: false,
		},
		{
			name:   "global true",
			repo:   nil,
			global: &config.Config{CI: config.CIConfig{UpsertComments: true}},
			want:   true,
		},
		{
			name:   "global false",
			repo:   nil,
			global: &config.Config{CI: config.CIConfig{UpsertComments: false}},
			want:   false,
		},
		{
			name: "repo true overrides global false",
			repo: &config.RepoConfig{
				CI: config.RepoCIConfig{UpsertComments: new(true)},
			},
			global: &config.Config{CI: config.CIConfig{UpsertComments: false}},
			want:   true,
		},
		{
			name: "repo false overrides global true",
			repo: &config.RepoConfig{
				CI: config.RepoCIConfig{UpsertComments: new(false)},
			},
			global: &config.Config{CI: config.CIConfig{UpsertComments: true}},
			want:   false,
		},
		{
			name: "repo nil falls through to global",
			repo: &config.RepoConfig{
				CI: config.RepoCIConfig{UpsertComments: nil},
			},
			global: &config.Config{CI: config.CIConfig{UpsertComments: true}},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := config.ResolveCIUpsertComments(tt.repo, tt.global)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveCIReasoning(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		got, err := config.ResolveCIReasoning("high", nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "thorough", got)
	})

	t.Run("repo override wins", func(t *testing.T) {
		got, err := config.ResolveCIReasoning("", &config.RepoConfig{
			CI: config.RepoCIConfig{Reasoning: "medium"},
		}, &config.Config{})
		require.NoError(t, err)
		assert.Equal(t, "medium", got)
	})

	t.Run("invalid repo config falls back to default", func(t *testing.T) {
		got, err := config.ResolveCIReasoning("", &config.RepoConfig{
			CI: config.RepoCIConfig{Reasoning: "nope"},
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, "thorough", got)
	})
}

func TestCIGitHubClient_UsesGHAuthTokenFallback(t *testing.T) {
	installFakeGHAuthToken(t, "gh-auth-token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	client, err := ciGitHubClient()
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestCIGitHubClient_UsesGitHubAPIURLForHostname(t *testing.T) {
	installFakeGHAuthToken(t, "enterprise-token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_HOST", "")
	t.Setenv("GITHUB_API_URL", "https://ghe.example.com/api/v3/")

	client, err := ciGitHubClient()
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestResolveCIMinSeverity(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		got, err := config.ResolveCIMinSeverity("HIGH", nil, nil)
		require.NoError(t, err)
		assert.Equal(t, "high", got)
	})

	t.Run("repo override beats global", func(t *testing.T) {
		got, err := config.ResolveCIMinSeverity("", &config.RepoConfig{
			CI: config.RepoCIConfig{MinSeverity: "medium"},
		}, &config.Config{
			CI: config.CIConfig{MinSeverity: "high"},
		})
		require.NoError(t, err)
		assert.Equal(t, "medium", got)
	})

	t.Run("invalid repo config falls through to valid global", func(t *testing.T) {
		got, err := config.ResolveCIMinSeverity("", &config.RepoConfig{
			CI: config.RepoCIConfig{MinSeverity: "bogus"},
		}, &config.Config{
			CI: config.CIConfig{MinSeverity: "critical"},
		})
		require.NoError(t, err)
		assert.Equal(t, "critical", got)
	})
}

func TestResolveCISynthesisAgent(t *testing.T) {
	got := config.ResolveCISynthesisAgent("", &config.RepoConfig{}, &config.Config{
		CI: config.CIConfig{SynthesisAgent: "gemini"},
	})
	assert.Equal(t, "gemini", got)

	got = config.ResolveCISynthesisAgent("codex", nil, &config.Config{
		CI: config.CIConfig{SynthesisAgent: "gemini"},
	})
	assert.Equal(t, "codex", got)
}

func TestSplitTrimmed(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b , c", []string{"a", "b", "c"}},
		{"single", []string{"single"}},
		{" , , ", nil},
	}
	for _, tt := range tests {
		got := splitTrimmed(tt.in)
		assert.Len(t, tt.want, len(got))
		for i := range got {
			assert.Equal(t, got[i], tt.want[i])
		}
	}
}
