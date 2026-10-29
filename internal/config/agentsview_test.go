package config

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentsViewConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		url   string
		valid bool
	}{
		{"https://usage.example/base", true},
		{"http://localhost:8080", true},
		{"", true},
		{"/relative", false},
		{"ftp://usage.example", false},
		{"https:///path", false},
		{"https://user:secret@usage.example", false},
		{"https://usage.example?x=y", false},
		{"https://usage.example#anchor", false},
		{"https://usage.example?", false},
		{"https://usage.example#", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[agentsview]\nurl = "+`"`+tc.url+`"`+"\n"), 0o600))
			_, err := LoadGlobalFrom(path)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "agentsview")
			}
		})
	}
}

func TestAgentsViewConflictAndScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[agentsview]\nurl='https://usage.example'\n[cost]\nendpoint='https://cost.example/{session_id}'\n"), 0o600))
	_, err := LoadGlobalFrom(path)
	require.ErrorContains(t, err, "cost.endpoint")
	cfg, err := LoadGlobalClientFrom(path)
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.DaemonConfigWarnings())
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte("[agentsview]\nurl='https://usage.example'\n"), 0o600))
	_, err = LoadRepoConfig(dir)
	require.ErrorContains(t, err, "global-only")
}

func TestAgentsViewTokenResolution(t *testing.T) {
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "env-token")
	c := AgentsViewConfig{URL: "https://usage.example"}
	token, err := c.ResolveToken()
	require.NoError(t, err)
	assert.Equal(t, "env-token", token)
	c.TokenFile = filepath.Join(t.TempDir(), "token")
	_, err = c.ResolveToken()
	require.ErrorContains(t, err, "agentsview.token_file")
	require.NoError(t, os.WriteFile(c.TokenFile, []byte(" file-token\n"), 0o600))
	token, err = c.ResolveToken()
	require.NoError(t, err)
	assert.Equal(t, "file-token", token)
}

func FuzzAgentsViewURL(f *testing.F) {
	for _, s := range []string{"https://usage.example/base", "https://u:p@host", "https://host?", "https://host#", "http://[::1]:80", "../x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c := AgentsViewConfig{URL: s}
		if s == "" || c.Validate(CostConfig{}) != nil {
			return
		}
		u, err := url.Parse(s)
		require.NoError(t, err)
		assert.Contains(t, []string{"http", "https"}, u.Scheme)
		assert.NotEmpty(t, u.Hostname())
		assert.Nil(t, u.User)
		assert.Empty(t, u.RawQuery)
		assert.False(t, u.ForceQuery)
		assert.Empty(t, u.Fragment)
	})
}

func TestAgentsViewTokenFileLoadAndHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "env-token")
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[agentsview]\nurl='https://usage.example'\ntoken_file='~/token'\n"), 0o600))
	_, err := LoadGlobalFrom(path)
	require.ErrorContains(t, err, "agentsview.token_file")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token"), []byte("home-token\n"), 0o600))
	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	token, err := cfg.AgentsView.ResolveToken()
	require.NoError(t, err)
	assert.Equal(t, "home-token", token)
	assert.True(t, IsValidKey("agentsview.url"))
	assert.True(t, IsGlobalKey("agentsview.token_file"))
	assert.True(t, IsSensitiveKey("agentsview.token_file"))
	cfg.Cost.Endpoint = "https://usage.example/{session_id}"
	require.ErrorContains(t, cfg.Validate(), "cost.endpoint")
}

func TestAgentsViewRepoRefRejected(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".roborev.toml", "[agentsview]\nurl='https://usage.example'\n")
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@example.com")
	execGit(t, dir, "config", "user.name", "Test")
	execGit(t, dir, "add", ".roborev.toml")
	execGit(t, dir, "commit", "-m", "configure usage")
	_, err := LoadRepoConfigFromRef(dir, "HEAD")
	require.ErrorContains(t, err, "global-only")
}
