package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveCIRewrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global bool
		repo   *RepoConfig
		want   bool
	}{
		{name: "default"},
		{name: "global on", global: true, want: true},
		{name: "repo inherits", global: true, repo: &RepoConfig{}, want: true},
		{name: "repo off", global: true, repo: &RepoConfig{CI: RepoCIConfig{Rewrite: new(false)}}},
		{name: "repo on", repo: &RepoConfig{CI: RepoCIConfig{Rewrite: new(true)}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.CI.Rewrite = tc.global
			assert.Equal(t, tc.want, ResolveCIRewrite(tc.repo, cfg))
		})
	}
}
