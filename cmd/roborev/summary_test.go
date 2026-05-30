package main

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestRepoLabels(t *testing.T) {
	tests := []struct {
		name  string
		repos []storage.RepoSummary
		want  []string
	}{
		{
			name: "unique basenames",
			repos: []storage.RepoSummary{
				{Path: "/home/user/project-a"},
				{Path: "/home/user/project-b"},
			},
			want: []string{"project-a", "project-b"},
		},
		{
			name: "duplicate basenames different parents",
			repos: []storage.RepoSummary{
				{Path: "/home/user/code/roborev"},
				{Path: "/home/user/worktrees/roborev"},
			},
			want: []string{"code/roborev", "worktrees/roborev"},
		},
		{
			name: "duplicate basenames same parent",
			repos: []storage.RepoSummary{
				{Path: "/a/worktrees/roborev"},
				{Path: "/b/worktrees/roborev"},
			},
			want: []string{"a/worktrees/roborev", "b/worktrees/roborev"},
		},
		{
			name: "uses Name field when set",
			repos: []storage.RepoSummary{
				{Name: "my-repo", Path: "/home/user/my-repo"},
				{Path: "/home/user/other-repo"},
			},
			want: []string{"my-repo", "other-repo"},
		},
		{
			name: "duplicate custom names preserved with path context",
			repos: []storage.RepoSummary{
				{Name: "app", Path: "/home/alice/src/app"},
				{Name: "app", Path: "/home/bob/src/app"},
			},
			want: []string{"alice/src/app", "bob/src/app"},
		},
		{
			name: "single repo",
			repos: []storage.RepoSummary{
				{Path: "/home/user/project"},
			},
			want: []string{"project"},
		},
		{
			name: "three duplicates with shared parents",
			repos: []storage.RepoSummary{
				{Path: "/x/a/repo"},
				{Path: "/y/a/repo"},
				{Path: "/z/b/repo"},
			},
			want: []string{"x/a/repo", "y/a/repo", "b/repo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repoLabels(tt.repos)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{"unix absolute", "/home/user/project", []string{"home", "user", "project"}},
		{"unix root child", "/project", []string{"project"}},
		{"relative", "a/b/c", []string{"a", "b", "c"}},
		{"single component", "project", []string{"project"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitPath(tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitPath_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only test")
	}
	tests := []struct {
		name string
		path string
		want []string
	}{
		{"drive root", `C:\Users\alice\repo`, []string{"Users", "alice", "repo"}},
		{"drive root child", `D:\repo`, []string{"repo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitPath(tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRepoLabels_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only test")
	}
	// Repos that differ only by drive letter — splitPath drops the volume,
	// so disambiguation must fall back to full paths.
	repos := []storage.RepoSummary{
		{Path: `C:\work\repo`},
		{Path: `D:\work\repo`},
	}
	got := repoLabels(repos)
	assert.Equal(t, []string{`C:\work\repo`, `D:\work\repo`}, got)
}
