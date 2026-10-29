package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
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

func TestSummaryFindingValidationBeforeDaemon(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--by", "agent"}, "require --findings"},
		{[]string{"--min-samples", "5"}, "require --findings"},
		{[]string{"--findings", "--by", "unknown"}, "invalid findings grouping"},
		{[]string{"--findings", "--min-samples", "-1"}, "positive integer"},
		{[]string{"--findings", "--min-samples", "0"}, "positive integer"},
	} {
		cmd := summaryCmd()
		cmd.SetArgs(tc.args)
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.want)
		assert.NotContains(t, err.Error(), "daemon")
	}
}

func TestPrintFindingSummary(t *testing.T) {
	cmd := summaryCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	f := storage.FindingStats{GroupBy: "severity", MinSamples: 5, LogicalReviews: 3, StructuredReviews: 2, Rows: []storage.FindingStatsRow{{Key: "high", Findings: 4, Labeled: 2, Positive: 1, Negative: 1, Unlabeled: 2, RateState: "below_minimum", FailingReviews: 1, ClosedReviews: 1, Cost: storage.CostAggregate{TotalUSD: 3, JobsTotal: 2, JobsWithCost: 1}, CostPerPositive: new(3.0)}}}
	printFindingSummary(cmd, &f)
	assert.Contains(t, out.String(), "2/3")
	assert.Contains(t, out.String(), "n<5")
	assert.Contains(t, out.String(), "1/1")
	assert.Contains(t, out.String(), "≥$3.00 (1/2 priced)")
	out.Reset()
	f.Rows[0].Cost.JobsWithCost = 0
	printFindingSummary(cmd, &f)
	assert.NotContains(t, out.String(), "Retained spend")
	assert.NotContains(t, out.String(), "Cost/positive")
}

func TestSummaryFindingRequest(t *testing.T) {
	for _, tc := range []struct {
		group   string
		members *storage.MemberStats
	}{
		{group: "agent"},
		{group: "member", members: &storage.MemberStats{Rows: []storage.MemberStatsRow{}}},
	} {
		t.Run(tc.group, func(t *testing.T) {
			assert := assert.New(t)
			queries := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/ping" {
					assert.NoError(json.MarshalWrite(w, daemon.PingInfo{OK: true, Service: "roborev"}))
					return
				}
				assert.Equal("/api/summary", r.URL.Path)
				queries <- r.URL.RawQuery
				assert.NoError(json.MarshalWrite(w, storage.Summary{Findings: &storage.FindingStats{GroupBy: tc.group, MinSamples: 2, Members: tc.members}}))
			}))
			defer server.Close()
			patchServerAddr(t, server.URL)
			old := parsedServerEndpoint
			parsedServerEndpoint = nil
			t.Cleanup(func() { parsedServerEndpoint = old })
			cmd := summaryCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"--all", "--findings", "--by", tc.group, "--min-samples", "2"})
			require.NoError(t, cmd.Execute())
			query := <-queries
			assert.Contains(query, "findings=true")
			assert.Contains(query, "by="+tc.group)
			assert.Contains(query, "min_samples=2")
			assert.Contains(out.String(), "Finding outcomes by "+tc.group)
			if tc.members != nil {
				assert.Contains(out.String(), "Panel member contribution")
			}
		})
	}
}

func TestPrintMemberFindingSummary(t *testing.T) {
	cmd := summaryCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	f := storage.FindingStats{GroupBy: "member", Members: &storage.MemberStats{SkippedPanelRuns: 1, SkippedMemberRuns: 2, Rows: []storage.MemberStatsRow{{RepoPath: "/tmp/sample", PanelName: "quality", MemberName: "first", Runs: 3, AttributedRuns: 2, SkippedRuns: 1, StructuredRuns: 1, FindingsRaised: 4, Cited: 3, Agreed: 2, Solo: 1, Cost: storage.CostAggregate{TotalUSD: 2, JobsTotal: 2, JobsWithCost: 1}, SkippedCost: storage.CostAggregate{TotalUSD: 7, JobsTotal: 1, JobsWithCost: 1}, CostPerCited: new(2.0 / 3), CostPerAgreed: new(1.0)}}}}
	printFindingSummary(cmd, &f)
	a := assert.New(t)
	a.Contains(out.String(), "Panel member contribution")
	a.Contains(out.String(), "quality")
	a.Contains(out.String(), "first")
	a.Contains(out.String(), "Structured runs")
	a.Contains(out.String(), "Cost/cited")
	a.Contains(out.String(), "≥$2.00 (1/2 priced)")
	a.Contains(out.String(), "1 panel runs, 2 member runs excluded")
	a.Contains(out.String(), "Solo labels unavailable")
	a.Contains(out.String(), "Skipped spend")
	out.Reset()
	f.Members.Rows[0].Cost.JobsWithCost = 0
	f.Members.Rows[0].SkippedCost.JobsWithCost = 0
	printFindingSummary(cmd, &f)
	a.NotContains(out.String(), "Cost/cited")
	a.NotContains(out.String(), "Skipped spend")
}
