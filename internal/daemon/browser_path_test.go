package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBrowserPath(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		basePath     string
		wantPath     string
		wantRedirect bool
		wantErr      string
	}{
		{name: "root deployment root", path: "/", wantPath: "/"},
		{name: "root deployment api", path: "/api/status", wantPath: "/api/status"},
		{name: "exact prefix redirects", path: "/roborev-ci", basePath: "/roborev-ci", wantPath: "/", wantRedirect: true},
		{name: "prefix root", path: "/roborev-ci/", basePath: "/roborev-ci", wantPath: "/"},
		{name: "prefix api", path: "/roborev-ci/api/status", basePath: "/roborev-ci", wantPath: "/api/status"},
		{name: "near match is outside", path: "/roborev-cinema", basePath: "/roborev-ci", wantErr: "outside"},
		{name: "other path is outside", path: "/reviews", basePath: "/roborev-ci", wantErr: "outside"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPath, redirect, err := normalizeBrowserPath(tt.path, tt.basePath)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, gotPath)
			assert.Equal(t, tt.wantRedirect, redirect)
		})
	}
}

func TestJoinBrowserPath(t *testing.T) {
	assert.Equal(t, "/", joinBrowserPath("", "/"))
	assert.Equal(t, "/reviews", joinBrowserPath("", "/reviews"))
	assert.Equal(t, "/host.example/reviews", joinBrowserPath("", "//host.example/reviews"))
	assert.Equal(t, "/host.example/reviews", joinBrowserPath("", `\\host.example/reviews`))
	assert.Equal(t, "/roborev-ci/", joinBrowserPath("/roborev-ci", "/"))
	assert.Equal(t, "/roborev-ci/reviews/42", joinBrowserPath("/roborev-ci", "/reviews/42"))
}
