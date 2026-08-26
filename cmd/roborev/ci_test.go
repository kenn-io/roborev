package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	glpkg "go.kenn.io/roborev/internal/gitlab"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/testutil"
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

func installFakeGlabAuthToken(t *testing.T, token string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping fake glab helper on Windows")
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "glab")
	script := "#!/bin/sh\nif [ \"$1\" = \"auth\" ] && [ \"$2\" = \"token\" ]; then\n  printf '%s\\n' " + "'" + token + "'\n  exit 0\nfi\nexit 1\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// clearForgeCIEnv removes the CI environment variables the forge detection
// looks at so tests are not affected by the ambient environment.
func clearForgeCIEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_EVENT_PATH", "GITHUB_REF",
		"GITLAB_CI", "CI_PROJECT_PATH", "CI_MERGE_REQUEST_PROJECT_PATH",
		"CI_MERGE_REQUEST_IID",
		"CI_COMMIT_SHA", "CI_COMMIT_BEFORE_SHA", "CI_COMMIT_BRANCH",
		"CI_MERGE_REQUEST_DIFF_BASE_SHA", "CI_MERGE_REQUEST_TARGET_BRANCH_SHA",
		"CI_MERGE_REQUEST_SOURCE_BRANCH_SHA", "CI_DEFAULT_BRANCH",
		"CI_SERVER_URL", "GITLAB_HOST", "GL_HOST", "GITLAB_TOKEN", "GL_TOKEN",
	} {
		t.Setenv(key, "")
	}
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
		"--gl-repo",
		"--gl-host",
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
		{"OptionLikeRef", []string{"review", "--ref=--format=%H"}, "--ref must not start with '-'", false},
		{"RequiresRef", []string{"review"}, "auto-detection", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Review type validation may need repository configuration, so every
			// case runs in an explicit test repository. Build sandboxes copy the
			// source without its .git directory.
			t.Chdir(testutil.InitTestRepo(t).Root)
			if tt.clearEnv {
				clearForgeCIEnv(t)
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

func TestDetectCIForge(t *testing.T) {
	tests := []struct {
		name          string
		opts          ciReviewOpts
		githubActions string
		gitlabCI      string
		want          ciForge
		wantErr       string
	}{
		{
			name: "DefaultsToGitHub",
			want: ciForgeGitHub,
		},
		{
			name: "GHRepoFlag",
			opts: ciReviewOpts{ghRepo: "owner/repo"},
			want: ciForgeGitHub,
		},
		{
			name: "GLRepoFlag",
			opts: ciReviewOpts{glRepo: "group/project"},
			want: ciForgeGitLab,
		},
		{
			name:     "GLRepoFlagBeatsGitHubActionsEnv",
			opts:     ciReviewOpts{glRepo: "group/project"},
			gitlabCI: "true", githubActions: "true",
			want: ciForgeGitLab,
		},
		{
			name:          "GHRepoFlagBeatsGitLabCIEnv",
			opts:          ciReviewOpts{ghRepo: "owner/repo"},
			gitlabCI:      "true",
			githubActions: "",
			want:          ciForgeGitHub,
		},
		{
			// Both indicators present is the injection case: a GitLab
			// pipeline starter can set GITHUB_ACTIONS=true as a pipeline
			// variable, while GITLAB_CI inside real GitHub Actions can only
			// come from committed workflow code. The GitLab indicator is the
			// one that cannot be forged from outside the job script, so it
			// wins — flipping to the GitHub client would resolve the API
			// origin from equally injectable GITHUB_API_URL.
			name:          "GitLabCIEnvBeatsGitHubActionsEnv",
			githubActions: "true",
			gitlabCI:      "true",
			want:          ciForgeGitLab,
		},
		{
			name: "GLHostFlag",
			opts: ciReviewOpts{glHost: "https://gitlab.example.com"},
			want: ciForgeGitLab,
		},
		{
			// The origin pin must pin forge selection too: an injected
			// GITHUB_ACTIONS=true must not steer a --gl-host run to the
			// GitHub client, which the pin never touches.
			name:          "GLHostFlagBeatsGitHubActionsEnv",
			opts:          ciReviewOpts{glHost: "https://gitlab.example.com"},
			githubActions: "true",
			want:          ciForgeGitLab,
		},
		{
			name: "GHRepoWithGLHostRejected",
			opts: ciReviewOpts{
				ghRepo: "owner/repo",
				glHost: "https://gitlab.example.com",
			},
			wantErr: "mutually exclusive",
		},
		{
			name:     "GitLabCIEnv",
			gitlabCI: "true",
			want:     ciForgeGitLab,
		},
		{
			name:     "GitLabCIEnvNumericTruthy",
			gitlabCI: "1",
			want:     ciForgeGitLab,
		},
		{
			name:     "GitLabCIEnvFalseFallsBackToGitHub",
			gitlabCI: "false",
			want:     ciForgeGitHub,
		},
		{
			name:    "BothRepoFlagsRejected",
			opts:    ciReviewOpts{ghRepo: "owner/repo", glRepo: "group/project"},
			wantErr: "mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearForgeCIEnv(t)
			t.Setenv("GITHUB_ACTIONS", tt.githubActions)
			t.Setenv("GITLAB_CI", tt.gitlabCI)

			got, err := detectCIForge(tt.opts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCIReviewCmd_RepoFlagsMutuallyExclusive(t *testing.T) {
	tests := []struct {
		name   string
		flags  []string
		wanted []string
	}{
		{
			name: "GHRepoWithGLRepo",
			flags: []string{
				"--gh-repo", "owner/repo", "--gl-repo", "group/project",
			},
			wanted: []string{"gh-repo", "gl-repo"},
		},
		{
			name: "GHRepoWithGLHost",
			flags: []string{
				"--gh-repo", "owner/repo",
				"--gl-host", "https://gitlab.example.com",
			},
			wanted: []string{"gh-repo", "gl-host"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ciCmd()
			cmd.SetArgs(append([]string{"review", "--ref", "abc"}, tt.flags...))
			var buf strings.Builder
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)

			err := cmd.Execute()
			require.Error(t, err)
			for _, want := range tt.wanted {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestDetectGitLabGitRef(t *testing.T) {
	tests := []struct {
		name             string
		commitSHA        string
		sourceBranchSHA  string
		diffBaseSHA      string
		targetBranchSHA  string
		commitBeforeSHA  string
		want             string
		wantErrSubstring string
	}{
		{
			name:        "MergeRequestDiffBase",
			commitSHA:   "head111",
			diffBaseSHA: "base000",
			want:        "base000..head111",
		},
		{
			// Merged results pipeline: CI_COMMIT_SHA is the
			// synthetic merge commit, so the source branch HEAD
			// must win to keep target changes out of the diff.
			name:            "SourceBranchSHAWinsOverCommitSHA",
			commitSHA:       "merged999",
			sourceBranchSHA: "source111",
			diffBaseSHA:     "base000",
			want:            "base000..source111",
		},
		{
			name:            "EmptySourceBranchSHAFallsThrough",
			commitSHA:       "head111",
			sourceBranchSHA: "",
			diffBaseSHA:     "base000",
			want:            "base000..head111",
		},
		{
			name:            "IgnoresZeroSourceBranchSHA",
			commitSHA:       "head111",
			sourceBranchSHA: "0000000000000000000000000000000000000000",
			diffBaseSHA:     "base000",
			want:            "base000..head111",
		},
		{
			// No base and no default branch to derive one from: the pushed
			// range is unknowable, so asking for --ref beats reviewing the tip.
			name:             "SourceBranchSHAWithoutBaseErrors",
			sourceBranchSHA:  "source111",
			wantErrSubstring: "--ref",
		},
		{
			name:            "DiffBaseWinsOverTargetBranch",
			commitSHA:       "head111",
			diffBaseSHA:     "base000",
			targetBranchSHA: "target999",
			want:            "base000..head111",
		},
		{
			name:            "FallsBackToTargetBranchSHA",
			commitSHA:       "head111",
			targetBranchSHA: "target999",
			want:            "target999..head111",
		},
		{
			name:            "FallsBackToCommitBeforeSHA",
			commitSHA:       "head111",
			commitBeforeSHA: "before777",
			want:            "before777..head111",
		},
		{
			// A zero before-SHA is a first push. It is still not a usable base,
			// and with no default branch the range cannot be reconstructed.
			name:             "IgnoresZeroCommitBeforeSHA",
			commitSHA:        "head111",
			commitBeforeSHA:  "0000000000000000000000000000000000000000",
			wantErrSubstring: "first push",
		},
		{
			name:             "NoBaseErrorsWithoutDefaultBranch",
			commitSHA:        "head111",
			wantErrSubstring: "CI_DEFAULT_BRANCH",
		},
		{
			name:             "MissingCommitSHA",
			wantErrSubstring: "CI_COMMIT_SHA",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearForgeCIEnv(t)
			t.Setenv("CI_COMMIT_SHA", tt.commitSHA)
			t.Setenv("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA", tt.sourceBranchSHA)
			t.Setenv("CI_MERGE_REQUEST_DIFF_BASE_SHA", tt.diffBaseSHA)
			t.Setenv("CI_MERGE_REQUEST_TARGET_BRANCH_SHA", tt.targetBranchSHA)
			t.Setenv("CI_COMMIT_BEFORE_SHA", tt.commitBeforeSHA)

			// A temp dir that is not a repo makes merge-base
			// resolution fail, exercising the tip fallback.
			got, err := detectGitLabGitRef(t.TempDir())
			if tt.wantErrSubstring != "" {
				require.ErrorContains(t, err, tt.wantErrSubstring)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDetectGitRefForForge_UsesForgeSpecificEnv(t *testing.T) {
	clearForgeCIEnv(t)
	setupFakeGitHubEvent(t, map[string]any{
		"pull_request": map[string]any{
			"base": map[string]string{"sha": "ghbase"},
			"head": map[string]string{"sha": "ghhead"},
		},
	})
	t.Setenv("CI_COMMIT_SHA", "glhead")
	t.Setenv("CI_MERGE_REQUEST_DIFF_BASE_SHA", "glbase")

	ghRef, err := detectGitRefForForge(ciForgeGitHub, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "ghbase..ghhead", ghRef)

	glRef, err := detectGitRefForForge(ciForgeGitLab, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "glbase..glhead", glRef)
}

func TestTrustedCIRepoConfigRefUsesPreReviewCommit(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.HeadSHA()
	head := repo.CommitFile("feature.txt", "feature", "feature work")

	got, err := trustedCIRepoConfigRef(repo.Path(), base+".."+head)
	require.NoError(t, err)
	assert.Equal(t, base, got)

	got, err = trustedCIRepoConfigRef(repo.Path(), head)
	require.NoError(t, err)
	assert.Equal(t, base, got)
}

// TestDetectGitLabGitRef_DivergedBranches covers the fallback bases, which are
// branch tips rather than diff bases. When the target branch advanced after the
// branches diverged, comparing tips would report target-only commits as
// removals, so the range must start at the merge base instead.
func TestDetectGitLabGitRef_DivergedBranches(t *testing.T) {
	tests := []struct {
		name    string
		baseEnv string
	}{
		{"TargetBranchSHA", "CI_MERGE_REQUEST_TARGET_BRANCH_SHA"},
		{"CommitBeforeSHA", "CI_COMMIT_BEFORE_SHA"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			repo := testutil.NewTestRepoWithCommit(t)
			mergeBase := repo.HeadSHA()
			defaultBranch := strings.TrimSpace(
				repo.RevParse("--abbrev-ref", "HEAD"))

			// Source branch: one commit on top of the merge base.
			repo.CheckoutNewBranch("feature")
			head := repo.CommitFile("feature.txt", "feature", "feature work")

			// Target branch advances after the divergence.
			repo.CheckoutBranch(defaultBranch)
			targetTip := repo.CommitFile("target.txt", "target", "target work")
			assert.NotEqual(mergeBase, targetTip)

			clearForgeCIEnv(t)
			t.Setenv("CI_COMMIT_SHA", head)
			t.Setenv(tt.baseEnv, targetTip)

			got, err := detectGitLabGitRef(repo.Path())
			require.NoError(t, err)
			assert.Equal(mergeBase+".."+head, got)
			assert.NotContains(got, targetTip)
		})
	}
}

// TestDetectGitLabGitRef_MissingBaseErrors covers the shallow-clone case. When a
// base was named but is not in the clone, neither way out is good: base..head
// fails on the missing commit in every later git operation, and narrowing to
// head alone is worse still, because a single ref is diffed with `git show` — one
// commit — so a multi-commit merge request would be reviewed as its last commit
// and could still post a passing verdict. Failing with an actionable message is
// the honest option.
//
// The diff-base case matters most: it skips gitLabMergeBase entirely, so the
// presence check is the only thing standing between a shallow clone and a wrong
// answer. It is also the path every normal merge request pipeline takes, where
// GitLab clones with GIT_DEPTH=20 by default.
func TestDetectGitLabGitRef_MissingBaseErrors(t *testing.T) {
	const absentSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	baseKeys := []string{
		"CI_MERGE_REQUEST_DIFF_BASE_SHA",
		"CI_MERGE_REQUEST_TARGET_BRANCH_SHA",
		"CI_COMMIT_BEFORE_SHA",
	}

	for _, baseKey := range baseKeys {
		t.Run(baseKey, func(t *testing.T) {
			assert := assert.New(t)
			repo := testutil.NewTestRepoWithCommit(t)
			head := repo.HeadSHA()

			clearForgeCIEnv(t)
			t.Setenv("CI_COMMIT_SHA", head)
			t.Setenv(baseKey, absentSHA)

			got, err := detectGitLabGitRef(repo.Path())
			require.Error(t, err)
			assert.Empty(got,
				"no ref may be returned when the range cannot be covered")
			assert.Contains(err.Error(), absentSHA)
			assert.Contains(err.Error(), "GIT_DEPTH",
				"the error must tell the operator how to fix it")
		})
	}
}

// A branch's first push leaves every base candidate empty or zero, but it still
// carries a range: CI_COMMIT_BEFORE_SHA is zero only on the first push, and merge
// request pipelines always set the diff base. Returning head alone would diff a
// multi-commit push with `git show` and post a verdict covering its last commit,
// so the range is reconstructed from the default branch instead.
func TestDetectGitLabGitRef_FirstPushUsesDefaultBranch(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	mergeBase := repo.HeadSHA()
	defaultBranch := strings.TrimSpace(repo.RevParse("--abbrev-ref", "HEAD"))

	// Two commits on a new branch, as a first push would carry.
	repo.CheckoutNewBranch("feature")
	repo.CommitFile("one.txt", "one", "first of the push")
	head := repo.CommitFile("two.txt", "two", "second of the push")

	clearForgeCIEnv(t)
	t.Setenv("CI_COMMIT_SHA", head)
	t.Setenv("CI_COMMIT_BEFORE_SHA", zeroSHA)
	t.Setenv("CI_DEFAULT_BRANCH", defaultBranch)

	got, err := detectGitLabGitRef(repo.Path())
	require.NoError(t, err)
	assert.Equal(t, mergeBase+".."+head, got,
		"the whole push must be reviewed, not just its last commit")
}

// Head already contained in the default branch means the push added no commits
// of its own. Reviewing the commit it points at would report on code that is
// already on the default branch and was reviewed when it landed there, so the
// run reports that there is nothing to review instead.
func TestDetectGitLabGitRef_FirstPushContainedInDefaultBranch(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.HeadSHA()
	defaultBranch := strings.TrimSpace(repo.RevParse("--abbrev-ref", "HEAD"))

	clearForgeCIEnv(t)
	t.Setenv("CI_COMMIT_SHA", head)
	t.Setenv("CI_DEFAULT_BRANCH", defaultBranch)

	got, err := detectGitLabGitRef(repo.Path())
	require.ErrorIs(t, err, errNoChangesToReview)
	assert.Empty(t, got)
}

// The default branch's own first push — a brand-new repository's initial
// pipeline — reaches the first-push fallback with the pushed branch equal to
// CI_DEFAULT_BRANCH, so the merge base of the default branch with head is head
// itself. Returning head there would review one commit of a multi-commit
// initial push and imply the rest passed. A single-commit initial push has no
// such gap: the commit is the whole push.
func TestDetectGitLabGitRef_DefaultBranchFirstPush(t *testing.T) {
	t.Run("MultiCommitErrors", func(t *testing.T) {
		assert := assert.New(t)
		repo := testutil.NewTestRepoWithCommit(t)
		repo.CommitFile("second.txt", "two", "second of the push")
		head := repo.HeadSHA()
		branch := strings.TrimSpace(repo.RevParse("--abbrev-ref", "HEAD"))

		clearForgeCIEnv(t)
		t.Setenv("CI_COMMIT_SHA", head)
		t.Setenv("CI_COMMIT_BEFORE_SHA", zeroSHA)
		t.Setenv("CI_DEFAULT_BRANCH", branch)
		t.Setenv("CI_COMMIT_BRANCH", branch)

		got, err := detectGitLabGitRef(repo.Path())
		require.Error(t, err)
		assert.Empty(got,
			"a multi-commit initial push must not be narrowed to its tip")
		assert.Contains(err.Error(), "initial push")
		assert.Contains(err.Error(), "--ref",
			"the error must tell the operator how to proceed")
	})

	t.Run("SingleCommitReviewsHead", func(t *testing.T) {
		repo := testutil.NewTestRepoWithCommit(t)
		head := repo.HeadSHA()
		branch := strings.TrimSpace(repo.RevParse("--abbrev-ref", "HEAD"))

		clearForgeCIEnv(t)
		t.Setenv("CI_COMMIT_SHA", head)
		t.Setenv("CI_COMMIT_BEFORE_SHA", zeroSHA)
		t.Setenv("CI_DEFAULT_BRANCH", branch)
		t.Setenv("CI_COMMIT_BRANCH", branch)

		got, err := detectGitLabGitRef(repo.Path())
		require.NoError(t, err)
		assert.Equal(t, head, got)
	})

	// A GIT_DEPTH=1 checkout of a multi-commit initial push reports a
	// rev-list count of one: the graft hides the parent. The raw commit
	// object still names it, so the cut must be detected and the run must
	// fail with the shallow-clone guidance rather than review the tip and
	// imply the rest passed.
	t.Run("ShallowMultiCommitErrors", func(t *testing.T) {
		assert := assert.New(t)
		src := testutil.NewTestRepoWithCommit(t)
		head := src.CommitFile("second.txt", "two", "second of the push")
		branch := strings.TrimSpace(src.RevParse("--abbrev-ref", "HEAD"))

		cloneDir := filepath.Join(t.TempDir(), "clone")
		// file:// forces the transport path — a plain local-path clone
		// copies everything and ignores --depth.
		src.Run("clone", "--depth", "1", "file://"+src.Path(), cloneDir)

		clearForgeCIEnv(t)
		t.Setenv("CI_COMMIT_SHA", head)
		t.Setenv("CI_COMMIT_BEFORE_SHA", zeroSHA)
		t.Setenv("CI_DEFAULT_BRANCH", branch)
		t.Setenv("CI_COMMIT_BRANCH", branch)

		got, err := detectGitLabGitRef(cloneDir)
		require.Error(t, err)
		assert.Empty(got,
			"a shallow clone must not pass off the tip as the whole push")
		assert.Contains(err.Error(), "GIT_DEPTH",
			"the error must tell the operator how to fix it")
	})

	// The same depth-1 checkout of a genuinely single-commit push must still
	// work: the fetched commit is a real root, so nothing was cut.
	t.Run("ShallowSingleCommitReviewsHead", func(t *testing.T) {
		src := testutil.NewTestRepoWithCommit(t)
		head := src.HeadSHA()
		branch := strings.TrimSpace(src.RevParse("--abbrev-ref", "HEAD"))

		cloneDir := filepath.Join(t.TempDir(), "clone")
		src.Run("clone", "--depth", "1", "file://"+src.Path(), cloneDir)

		clearForgeCIEnv(t)
		t.Setenv("CI_COMMIT_SHA", head)
		t.Setenv("CI_COMMIT_BEFORE_SHA", zeroSHA)
		t.Setenv("CI_DEFAULT_BRANCH", branch)
		t.Setenv("CI_COMMIT_BRANCH", branch)

		got, err := detectGitLabGitRef(cloneDir)
		require.NoError(t, err)
		assert.Equal(t, head, got)
	})
}

// Without a resolvable default branch there is no way to tell a one-commit push
// from a twelve-commit one, so the run must stop and ask rather than review the
// tip and imply the rest passed.
func TestDetectGitLabGitRef_FirstPushWithoutDefaultBranchErrors(t *testing.T) {
	tests := []struct {
		name          string
		defaultBranch string
		wantSubstring string
	}{
		{
			name:          "Unset",
			defaultBranch: "",
			wantSubstring: "CI_DEFAULT_BRANCH",
		},
		{
			name:          "NotInClone",
			defaultBranch: "no-such-branch",
			wantSubstring: "could not be resolved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			repo := testutil.NewTestRepoWithCommit(t)
			head := repo.HeadSHA()

			clearForgeCIEnv(t)
			t.Setenv("CI_COMMIT_SHA", head)
			t.Setenv("CI_COMMIT_BEFORE_SHA", zeroSHA)
			t.Setenv("CI_DEFAULT_BRANCH", tt.defaultBranch)

			got, err := detectGitLabGitRef(repo.Path())
			require.Error(t, err)
			assert.Empty(got)
			assert.Contains(err.Error(), tt.wantSubstring)
			assert.Contains(err.Error(), "--ref",
				"the error must tell the operator how to proceed")
		})
	}
}

// TestDetectGitLabGitRef_MissingHead covers the shallow merged-results checkout.
// CI_MERGE_REQUEST_SOURCE_BRANCH_SHA is the preferred head because CI_COMMIT_SHA
// is a synthetic merge commit there — but with GIT_DEPTH=1 the merge commit is the
// only thing fetched, so the source branch HEAD can be absent. Returning it would
// make every later git operation fail on a commit the clone does not have.
func TestDetectGitLabGitRef_MissingHead(t *testing.T) {
	const absentSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const otherAbsentSHA = "feedfacefeedfacefeedfacefeedfacefeedface"

	tests := []struct {
		name string
		// configure sets the CI env for repo and returns the expected ref.
		configure      func(t *testing.T, repo *testutil.TestRepo) string
		wantErrSubstrs []string
	}{
		{
			// The fallback has to be usable on its own: the range must name
			// the checked-out SHA, not the absent source branch HEAD.
			name: "FallsBackToCheckedOutSHA",
			configure: func(t *testing.T, repo *testutil.TestRepo) string {
				base := repo.HeadSHA()
				head := repo.CommitFile("feature.txt", "feature", "feature work")
				t.Setenv("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA", absentSHA)
				t.Setenv("CI_COMMIT_SHA", head)
				t.Setenv("CI_MERGE_REQUEST_DIFF_BASE_SHA", base)
				return base + ".." + head
			},
		},
		{
			// Guards the merged-results preference: when the source branch
			// HEAD is present it must still win over CI_COMMIT_SHA.
			name: "PresentSourceSHAStillWins",
			configure: func(t *testing.T, repo *testutil.TestRepo) string {
				merged := repo.HeadSHA()
				source := repo.CommitFile("feature.txt", "feature", "feature work")
				t.Setenv("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA", source)
				t.Setenv("CI_COMMIT_SHA", merged)
				// A diff base keeps this on the merge request path, so the
				// assertion stays about head preference. Had CI_COMMIT_SHA won,
				// head would be merged and the range would collapse.
				t.Setenv("CI_MERGE_REQUEST_DIFF_BASE_SHA", merged)
				return merged + ".." + source
			},
		},
		{
			// Neither candidate is in the clone, so there is nothing to
			// degrade to. An actionable shallow-clone error beats a ref that
			// fails deeper in the pipeline.
			name: "NeitherHeadPresentErrors",
			configure: func(t *testing.T, _ *testutil.TestRepo) string {
				t.Setenv("CI_MERGE_REQUEST_SOURCE_BRANCH_SHA", absentSHA)
				t.Setenv("CI_COMMIT_SHA", otherAbsentSHA)
				return ""
			},
			wantErrSubstrs: []string{absentSHA, otherAbsentSHA, "GIT_DEPTH"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)

			repo := testutil.NewTestRepoWithCommit(t)
			clearForgeCIEnv(t)
			want := tt.configure(t, repo)

			got, err := detectGitLabGitRef(repo.Path())
			if len(tt.wantErrSubstrs) > 0 {
				require.Error(t, err)
				for _, substr := range tt.wantErrSubstrs {
					assert.Contains(err.Error(), substr)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(want, got)
			assert.NotContains(got, absentSHA,
				"a head that is absent from the clone must not appear in the range")
		})
	}
}

// A base that resolves but has no common ancestor with head still compares tips:
// the commits exist, so the range is usable even though it is imprecise.
func TestDetectGitLabGitRef_UnrelatedBaseComparesTips(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	head := repo.HeadSHA()
	// A real commit object that shares no history with head, so merge-base
	// finds nothing while the object itself is present.
	unrelated := repo.UnrelatedCommit("unrelated root")

	clearForgeCIEnv(t)
	t.Setenv("CI_COMMIT_SHA", head)
	t.Setenv("CI_MERGE_REQUEST_TARGET_BRANCH_SHA", unrelated)

	got, err := detectGitLabGitRef(repo.Path())
	require.NoError(t, err)
	assert.Equal(t, unrelated+".."+head, got)
}

// TestDetectGitLabGitRef_DiffBaseSkipsMergeBase pins that the diff base is used
// verbatim: GitLab already resolved it against the target branch. The base here
// is the advanced target tip, which is not an ancestor of head, so resolving a
// merge base would visibly rewrite it.
func TestDetectGitLabGitRef_DiffBaseSkipsMergeBase(t *testing.T) {
	assert := assert.New(t)

	repo := testutil.NewTestRepoWithCommit(t)
	mergeBase := repo.HeadSHA()
	defaultBranch := strings.TrimSpace(repo.RevParse("--abbrev-ref", "HEAD"))

	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("feature.txt", "feature", "feature work")

	repo.CheckoutBranch(defaultBranch)
	targetTip := repo.CommitFile("target.txt", "target", "target work")

	clearForgeCIEnv(t)
	t.Setenv("CI_COMMIT_SHA", head)
	t.Setenv("CI_MERGE_REQUEST_DIFF_BASE_SHA", targetTip)

	got, err := detectGitLabGitRef(repo.Path())
	require.NoError(t, err)
	assert.Equal(targetTip+".."+head, got)
	assert.NotContains(got, mergeBase, "diff base must not be rewritten")
}

func TestDetectMRIID(t *testing.T) {
	tests := []struct {
		name    string
		iid     string
		want    int
		wantErr bool
	}{
		{"Valid", "42", 42, false},
		{"Whitespace", " 7 ", 7, false},
		{"Missing", "", 0, true},
		{"NotANumber", "abc", 0, true},
		{"Zero", "0", 0, true},
		{"Negative", "-3", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearForgeCIEnv(t)
			t.Setenv("CI_MERGE_REQUEST_IID", tt.iid)

			got, err := detectMRIID()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDetectGitLabProjectPath(t *testing.T) {
	tests := []struct {
		name          string
		mrProjectPath string
		projectPath   string
		want          string
	}{
		{
			name:        "BranchPipelineUsesProjectPath",
			projectPath: "group/project",
			want:        "group/project",
		},
		{
			// Fork MR pipelines run in the fork, so CI_PROJECT_PATH
			// is the source project while the MR IID belongs to the
			// target project.
			name:          "MRProjectPathWins",
			mrProjectPath: "group/project",
			projectPath:   "contributor/project",
			want:          "group/project",
		},
		{
			name:          "IgnoresBlankMRProjectPath",
			mrProjectPath: "   ",
			projectPath:   "group/project",
			want:          "group/project",
		},
		{
			name: "NothingSet",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearForgeCIEnv(t)
			t.Setenv("CI_MERGE_REQUEST_PROJECT_PATH", tt.mrProjectPath)
			t.Setenv("CI_PROJECT_PATH", tt.projectPath)

			assert.Equal(t, tt.want, detectGitLabProjectPath())
		})
	}
}

func TestPostGitLabCIComment_MissingProject(t *testing.T) {
	clearForgeCIEnv(t)

	err := postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "body", false, "", stubMRRef)
	require.ErrorContains(t, err, "--gl-repo")
}

func TestPostGitLabCIComment_MissingMRIID(t *testing.T) {
	clearForgeCIEnv(t)
	t.Setenv("CI_PROJECT_PATH", "group/project")

	err := postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "body", false, "", stubMRRef)
	require.ErrorContains(t, err, "CI_MERGE_REQUEST_IID")
}

// stubMRBase and stubMRHead are the merge request refs the notes stub reports,
// so posting tests can pass a range that satisfies the binding check.
const (
	stubMRBase = "ba5e0000000000000000000000000000000000ba"
	stubMRHead = "feedface00000000000000000000000000000000"
	stubMRRef  = stubMRBase + ".." + stubMRHead
)

// stubGitLabNotesAPI serves the minimal notes API surface `--comment` uses and
// records what the command sent.
type stubGitLabNotesAPI struct {
	existingNotes []map[string]any
	createdBodies []string
	updatedBodies []string
	requestPaths  []string
	// mrHeadSHA and mrBaseSHA are what the merge request lookup reports; the
	// range binding only runs when --pr was passed explicitly.
	mrHeadSHA string
	mrBaseSHA string
}

func (s *stubGitLabNotesAPI) start(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.EscapedPath()
			if !strings.Contains(path, "/notes") {
				// The merge request lookup behind the head binding.
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"iid": 7,
					"sha": s.mrHeadSHA,
					"diff_refs": map[string]any{
						"base_sha": s.mrBaseSHA,
						"head_sha": s.mrHeadSHA,
					},
				}))
				return
			}
			s.requestPaths = append(s.requestPaths, path)
			var payload struct {
				Body string `json:"body"`
			}
			switch r.Method {
			case http.MethodGet:
				notes := s.existingNotes
				if notes == nil {
					notes = []map[string]any{}
				}
				assert.NoError(t, json.NewEncoder(w).Encode(notes))
			case http.MethodPost:
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				s.createdBodies = append(s.createdBodies, payload.Body)
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.NewEncoder(w).Encode(
					map[string]any{"id": 999, "body": payload.Body}))
			case http.MethodPut:
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				s.updatedBodies = append(s.updatedBodies, payload.Body)
				assert.NoError(t, json.NewEncoder(w).Encode(
					map[string]any{"id": 42, "body": payload.Body}))
			default:
				http.NotFound(w, r)
			}
		}))
	t.Cleanup(srv.Close)

	if s.mrHeadSHA == "" {
		s.mrHeadSHA = stubMRHead
	}
	if s.mrBaseSHA == "" {
		s.mrBaseSHA = stubMRBase
	}

	clearForgeCIEnv(t)
	t.Setenv("CI_SERVER_URL", srv.URL)
	t.Setenv("GITLAB_TOKEN", "stub-token")
	t.Setenv("CI_PROJECT_PATH", "group/project")
	t.Setenv("CI_MERGE_REQUEST_IID", "7")
	return srv.URL
}

func TestPostCIReviewComment(t *testing.T) {
	zeroOutputCases := []struct {
		name   string
		result review.ReviewResult
	}{
		{
			name:   "unknown failure",
			result: review.ReviewResult{Status: review.ResultFailed, Error: "unknown failure"},
		},
		{
			name:   "quota failure",
			result: review.ReviewResult{Status: review.ResultFailed, Error: review.QuotaErrorPrefix + "quota"},
		},
		{
			name:   "transient failure",
			result: review.ReviewResult{Status: review.ResultFailed, Error: review.OutageErrorPrefix + "503"},
		},
		{
			name:   "unavailable failure",
			result: review.ReviewResult{Status: review.ResultFailed, Error: review.UnavailableErrorPrefix + "startup"},
		},
		{
			name:   "timeout cancellation",
			result: review.ReviewResult{Status: review.ResultFailed, Error: review.TimeoutErrorPrefix + "batch deadline"},
		},
		{
			name:   "completed whitespace",
			result: review.ReviewResult{Status: review.ResultDone, Output: " \n\t"},
		},
		{
			name: "completed empty-output placeholder",
			result: review.ReviewResult{
				Status: review.ResultDone,
				Output: "No review output generated",
			},
		},
	}

	for _, tt := range zeroOutputCases {
		t.Run(tt.name+" skips forge request", func(t *testing.T) {
			api := &stubGitLabNotesAPI{}
			api.start(t)

			err := postCIReviewComment(
				context.Background(), ciForgeGitLab, ciReviewOpts{},
				[]review.ReviewResult{tt.result}, "local diagnostic summary",
				false, "", stubMRRef,
			)
			require.NoError(t, err)
			assert.Empty(t, api.requestPaths)
			assert.Empty(t, api.createdBodies)
		})
	}

	t.Run("partial success posts once", func(t *testing.T) {
		api := &stubGitLabNotesAPI{}
		api.start(t)

		results := []review.ReviewResult{
			{Agent: "codex", Status: review.ResultFailed, Error: "agent failed"},
			{Agent: "gemini", Status: review.ResultDone, Output: "## Findings\n"},
		}
		comment := review.FormatRawBatchComment(results, stubMRHead)
		err := postCIReviewComment(
			context.Background(), ciForgeGitLab, ciReviewOpts{}, results,
			comment, false, "", stubMRRef,
		)
		require.NoError(t, err)
		require.Len(t, api.createdBodies, 1)
		assert.Contains(t, api.createdBodies[0], "## Findings")
		assert.NotContains(t, api.createdBodies[0], "agent failed")
	})

	t.Run("full success posts once", func(t *testing.T) {
		api := &stubGitLabNotesAPI{}
		api.start(t)

		results := []review.ReviewResult{{
			Agent: "codex", Status: review.ResultDone, Output: "## Review\nNo findings.",
		}}
		err := postCIReviewComment(
			context.Background(), ciForgeGitLab, ciReviewOpts{}, results,
			"complete review", false, "", stubMRRef,
		)
		require.NoError(t, err)
		require.Len(t, api.createdBodies, 1)
		assert.Contains(t, api.createdBodies[0], "complete review")
	})
}

func TestPostGitLabCIComment_CreatesNewNote(t *testing.T) {
	api := &stubGitLabNotesAPI{}
	api.start(t)

	require.NoError(t, postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "review body", false, "", stubMRRef))
	require.Len(t, api.createdBodies, 1)
	assert.Empty(t, api.updatedBodies)
	assert.Contains(t, api.createdBodies[0], glpkg.CommentMarker)
	assert.Contains(t, api.createdBodies[0], "review body")
}

func TestPostGitLabCIComment_UpsertUpdatesExistingNote(t *testing.T) {
	api := &stubGitLabNotesAPI{
		existingNotes: []map[string]any{
			{"id": 42, "body": glpkg.CommentMarker + "\nold"},
		},
	}
	api.start(t)

	require.NoError(t, postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "review body", true, "", stubMRRef))
	require.Len(t, api.updatedBodies, 1)
	assert.Empty(t, api.createdBodies)
	assert.Contains(t, api.updatedBodies[0], "review body")
}

func TestPostGitLabCIComment_WithoutUpsertIgnoresExistingNote(t *testing.T) {
	api := &stubGitLabNotesAPI{
		existingNotes: []map[string]any{
			{"id": 42, "body": glpkg.CommentMarker + "\nold"},
		},
	}
	api.start(t)

	require.NoError(t, postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "review body", false, "", stubMRRef))
	require.Len(t, api.createdBodies, 1)
	assert.Empty(t, api.updatedBodies)
}

func TestPostGitLabCIComment_UsesExplicitFlagsOverEnv(t *testing.T) {
	const mrHead = "feedface00000000000000000000000000000000"
	const mrBase = "ba5e0000000000000000000000000000000000ba"
	api := &stubGitLabNotesAPI{mrHeadSHA: mrHead, mrBaseSHA: mrBase}
	// start() sets CI_PROJECT_PATH=group/project and
	// CI_MERGE_REQUEST_IID=7; the flags must win over both.
	api.start(t)

	// An explicit --pr also turns on the head binding, so the reviewed head
	// has to be the one the merge request reports.
	require.NoError(t, postGitLabCIComment(
		context.Background(),
		ciReviewOpts{glRepo: "group/subgroup/project", pr: 11},
		"review body", false, "", mrBase+".."+mrHead))
	require.Len(t, api.createdBodies, 1)
	require.Len(t, api.requestPaths, 1)
	assert.Contains(t, api.requestPaths[0],
		"group%2Fsubgroup%2Fproject/merge_requests/11/notes")
}

func TestPostGitLabCIComment_UsesEnvProjectAndIID(t *testing.T) {
	api := &stubGitLabNotesAPI{}
	api.start(t)

	require.NoError(t, postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "review body", false, "", stubMRRef))
	require.Len(t, api.requestPaths, 1)
	assert.Contains(t, api.requestPaths[0],
		"group%2Fproject/merge_requests/7/notes")
}

func TestPostGitLabCIComment_PrefersMRTargetProject(t *testing.T) {
	api := &stubGitLabNotesAPI{}
	// start() sets CI_PROJECT_PATH=group/project; a fork pipeline would
	// report the fork there while the MR IID belongs to the target project.
	api.start(t)
	t.Setenv("CI_PROJECT_PATH", "contributor/project")
	t.Setenv("CI_MERGE_REQUEST_PROJECT_PATH", "group/project")

	require.NoError(t, postGitLabCIComment(
		context.Background(), ciReviewOpts{}, "review body", false, "", stubMRRef))
	require.Len(t, api.requestPaths, 1)
	assert.Contains(t, api.requestPaths[0],
		"group%2Fproject/merge_requests/7/notes")
	assert.NotContains(t, api.requestPaths[0], "contributor")
}

// TestPostGitLabCIComment_GLHostFlagBeatsEnv pins the origin pin. CI_SERVER_URL
// is a predefined variable a pipeline starter can override with a custom
// variable, while --gl-host comes from the job script, which they cannot
// rewrite — so the flag must win and the env-derived origin must never be
// contacted.
func TestPostGitLabCIComment_GLHostFlagBeatsEnv(t *testing.T) {
	pinned := &stubGitLabNotesAPI{}
	pinnedURL := pinned.start(t)
	decoy := &stubGitLabNotesAPI{}
	// Started second, so CI_SERVER_URL now points at the decoy.
	decoy.start(t)

	require.NoError(t, postGitLabCIComment(
		context.Background(),
		ciReviewOpts{glHost: pinnedURL},
		"review body", false, "", stubMRRef))
	require.Len(t, pinned.createdBodies, 1)
	assert.Empty(t, decoy.requestPaths,
		"the env-derived origin must not receive the token when --gl-host is set")
}

// A plaintext non-loopback origin would expose the token in transit, so the
// client refuses to be built at all — before any review work is spent.
func TestCIGitLabClient_RejectsPlaintextHTTPOrigin(t *testing.T) {
	clearForgeCIEnv(t)
	t.Setenv("GITLAB_TOKEN", "token")
	t.Setenv("CI_SERVER_URL", "http://gitlab.example.com")

	_, err := ciGitLabClient(context.Background(), "")
	require.ErrorContains(t, err, "https")
}

func TestCIGitLabClient_UsesGlabAuthTokenFallback(t *testing.T) {
	clearForgeCIEnv(t)
	installFakeGlabAuthToken(t, "glab-auth-token")

	client, err := ciGitLabClient(context.Background(), "")
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestCIGitLabClient_UsesCIServerURLForSelfHostedHost(t *testing.T) {
	clearForgeCIEnv(t)
	installFakeGlabAuthToken(t, "self-hosted-token")
	t.Setenv("CI_SERVER_URL", "https://gitlab.example.com")

	client, err := ciGitLabClient(context.Background(), "")
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestCIGitLabClient_MissingTokenErrors(t *testing.T) {
	clearForgeCIEnv(t)
	// Point PATH at an empty dir so no real glab CLI can answer.
	t.Setenv("PATH", t.TempDir())

	_, err := ciGitLabClient(context.Background(), "")
	require.ErrorContains(t, err, "GitLab authentication required")
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
		assert.Equal(t, "high", got)
	})

	t.Run("legacy explicit flag is unchanged", func(t *testing.T) {
		got, err := config.ResolveCIReasoning("thorough", nil, nil)
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

	client, err := ciGitHubClient(context.Background())
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestCIGitHubClient_UsesGitHubAPIURLForHostname(t *testing.T) {
	installFakeGHAuthToken(t, "enterprise-token")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_HOST", "")
	t.Setenv("GITHUB_API_URL", "https://ghe.example.com/api/v3/")

	client, err := ciGitHubClient(context.Background())
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

// TestResolveCIAnthropicAPIKey must not run in parallel: it mutates
// ANTHROPIC_API_KEY in the process environment.
func TestResolveCIAnthropicAPIKey(t *testing.T) {
	tests := []struct {
		name      string
		configKey string
		envKey    string
		want      string
	}{
		{
			name:      "config key wins over env",
			configKey: "sk-config",
			envKey:    "sk-env",
			want:      "sk-config",
		},
		{
			name:      "env used when config empty",
			configKey: "",
			envKey:    "sk-env",
			want:      "sk-env",
		},
		{
			name:      "blank config falls through to env",
			configKey: "   ",
			envKey:    " sk-env ",
			want:      "sk-env",
		},
		{
			name:      "empty when neither is set",
			configKey: "",
			envKey:    "",
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", tt.envKey)
			assert.Equal(t, tt.want,
				resolveCIAnthropicAPIKey(tt.configKey))
		})
	}
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

// --min-severity must pin the review-level threshold, not just the synthesis
// filter. In the documented MR-head worktree flow, .roborev.toml comes from the
// tree under review, so an author could otherwise set
// review_min_severity = "critical" and keep medium/high findings from ever
// being reported — leaving synthesis nothing to filter and a clean-looking pass.
func TestResolveCIReviewMinSeverity(t *testing.T) {
	tests := []struct {
		name        string
		flag        string
		repoSetting string
		want        string
	}{
		{
			name:        "FlagBeatsRepoConfig",
			flag:        "medium",
			repoSetting: "critical",
			want:        "medium",
		},
		{
			name:        "RepoConfigAppliesWithoutFlag",
			flag:        "",
			repoSetting: "critical",
			want:        "critical",
		},
		{
			name:        "EmptyWhenNeitherIsSet",
			flag:        "",
			repoSetting: "",
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			if tt.repoSetting != "" {
				repo.WriteFile(".roborev.toml",
					"review_min_severity = \""+tt.repoSetting+"\"\n")
			}

			got, err := resolveCIReviewMinSeverity(
				ciReviewOpts{minSeverity: tt.flag}, repo.Path(), nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// verifyMRRangeErr calls verifyGitLabMRRange and keeps only the error; the
// canonical range it returns is asserted separately where it matters.
func verifyMRRangeErr(
	ctx context.Context, opts ciReviewOpts, repoPath, gitRef string, recheck bool,
) error {
	_, err := verifyGitLabMRRange(ctx, opts, repoPath, gitRef, recheck)
	return err
}

// stubGitLabMRAPI serves the merge request lookup the head binding needs,
// alongside the notes endpoints, and records whether the note was posted.
type stubGitLabMRAPI struct {
	headSHA string
	baseSHA string
	posted  bool
	mrPaths []string
}

func (s *stubGitLabMRAPI) start(t *testing.T) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			path := r.URL.EscapedPath()
			switch {
			case strings.HasSuffix(path, "/notes") && r.Method == http.MethodPost:
				s.posted = true
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.NewEncoder(w).Encode(
					map[string]any{"id": 1, "body": "posted"}))
			case strings.HasSuffix(path, "/notes"):
				assert.NoError(t, json.NewEncoder(w).Encode([]map[string]any{}))
			default:
				s.mrPaths = append(s.mrPaths, path)
				// Routed by IID so a lookup of the wrong merge request — !0
				// when detection is skipped, say — cannot silently pass.
				if !strings.Contains(path, "/merge_requests/7") {
					http.NotFound(w, r)
					return
				}
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"iid": 7,
					"sha": s.headSHA,
					"diff_refs": map[string]any{
						"base_sha": s.baseSHA,
						"head_sha": s.headSHA,
					},
				}))
			}
		}))
	t.Cleanup(srv.Close)

	clearForgeCIEnv(t)
	t.Setenv("CI_SERVER_URL", srv.URL)
	t.Setenv("GITLAB_TOKEN", "stub-token")
	t.Setenv("CI_PROJECT_PATH", "group/project")
}

// An explicitly supplied --pr is the protected-branch flow, where --pr and
// --ref arrive as separate inputs and a pipeline trigger can choose them
// independently. The whole range has to match the merge request: a head-only
// check still lets a narrowed range post a passing verdict that covers one
// commit of many.
func TestVerifyGitLabMRRange(t *testing.T) {
	// buildRepo returns a repo whose feature branch has two commits on top of
	// the default branch, plus the merge base and the branch head.
	buildRepo := func(t *testing.T) (repo *testutil.TestRepo, base, head string) {
		t.Helper()
		repo = testutil.NewTestRepoWithCommit(t)
		base = repo.HeadSHA()
		repo.CheckoutNewBranch("feature")
		repo.CommitFile("one.txt", "one", "first")
		head = repo.CommitFile("two.txt", "two", "second")
		return repo, base, head
	}

	t.Run("ExactRangePasses", func(t *testing.T) {
		repo, base, head := buildRepo(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: base}
		api.start(t)

		require.NoError(t, verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7},
			repo.Path(), base+".."+head, false))
	})

	// A base earlier than the merge request's is refused too. It looks
	// harmless — reviewing more omits nothing — but a change the merge request
	// makes can cancel against one between the two bases, leaving a diff that
	// shows less than the merge request actually does.
	t.Run("WiderRangeFails", func(t *testing.T) {
		repo, base, head := buildRepo(t)
		// The merge request's base is one commit later than the range's.
		mrBase := strings.TrimSpace(repo.RevParse("HEAD~1"))
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: mrBase}
		api.start(t)

		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7},
			repo.Path(), base+".."+head, false)
		require.ErrorContains(t, err, mrBase)
	})

	// GitLab omits the diff base on a merge request it has not finished
	// preparing. Accepting any base there would let a narrowed range through,
	// so the run stops instead.
	t.Run("MissingMRBaseFails", func(t *testing.T) {
		repo, base, head := buildRepo(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: ""}
		api.start(t)

		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7},
			repo.Path(), base+".."+head, false)
		require.ErrorContains(t, err, "no diff base")
	})

	t.Run("NarrowedRangeFails", func(t *testing.T) {
		repo, base, head := buildRepo(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: base}
		api.start(t)

		// head~1 is inside the merge request, so this range hides its first
		// commit while still ending at the right head.
		narrowed := strings.TrimSpace(repo.RevParse("HEAD~1"))
		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7},
			repo.Path(), narrowed+".."+head, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), base,
			"the error must name the base the range should have started at")
	})

	t.Run("SingleRefFails", func(t *testing.T) {
		repo, base, head := buildRepo(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: base}
		api.start(t)

		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7}, repo.Path(), head, false)
		require.ErrorContains(t, err, "BASE..HEAD")
	})

	t.Run("EmptyRangeFails", func(t *testing.T) {
		repo, _, head := buildRepo(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: head}
		api.start(t)

		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7},
			repo.Path(), head+".."+head, false)
		require.Error(t, err)
	})

	t.Run("WrongHeadFails", func(t *testing.T) {
		repo, base, head := buildRepo(t)
		const otherHead = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
		api := &stubGitLabMRAPI{headSHA: otherHead, baseSHA: base}
		api.start(t)

		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{pr: 7},
			repo.Path(), base+".."+head, false)
		require.ErrorContains(t, err, otherHead)
	})
}

// A derived range gets the same check as a supplied one. The CI variables it
// comes from are overridable by whoever starts the pipeline, so trusting them
// would leave open the hole the flags close: injected variables could name a
// narrowed range and still post a passing note.
func TestVerifyGitLabMRRange_DerivedRangeIsChecked(t *testing.T) {
	build := func(t *testing.T) (repo *testutil.TestRepo, base, head string) {
		t.Helper()
		repo = testutil.NewTestRepoWithCommit(t)
		base = repo.HeadSHA()
		repo.CheckoutNewBranch("feature")
		repo.CommitFile("one.txt", "one", "first")
		head = repo.CommitFile("two.txt", "two", "second")
		return repo, base, head
	}

	t.Run("MatchingRangePasses", func(t *testing.T) {
		repo, base, head := build(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: base}
		api.start(t)
		t.Setenv("CI_MERGE_REQUEST_IID", "7")

		canonical, err := verifyGitLabMRRange(
			context.Background(), ciReviewOpts{}, repo.Path(),
			base+".."+head, false)
		require.NoError(t, err)
		assert.Equal(t, base+".."+head, canonical)
	})

	t.Run("NarrowedRangeFails", func(t *testing.T) {
		repo, base, head := build(t)
		api := &stubGitLabMRAPI{headSHA: head, baseSHA: base}
		api.start(t)
		t.Setenv("CI_MERGE_REQUEST_IID", "7")

		narrowed := strings.TrimSpace(repo.RevParse("HEAD~1"))
		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{}, repo.Path(),
			narrowed+".."+head, false)
		require.ErrorContains(t, err, base)
	})

	// A first-pass mismatch fails loudly even for a derived range: only the
	// recheck can prove a race, and exiting clean here would let overridden CI
	// variables produce a green job with nothing reviewed.
	t.Run("StaleHeadFailsOnFirstPass", func(t *testing.T) {
		repo, base, head := build(t)
		const movedTo = "feedface00000000000000000000000000000000"
		api := &stubGitLabMRAPI{headSHA: movedTo, baseSHA: base}
		api.start(t)
		t.Setenv("CI_MERGE_REQUEST_IID", "7")

		err := verifyMRRangeErr(
			context.Background(), ciReviewOpts{}, repo.Path(),
			base+".."+head, false)
		require.Error(t, err)
		require.NotErrorIs(t, err, errMRHeadMoved)
		assert.Contains(t, err.Error(), movedTo)
	})
}

// A run that can never post fails during preflight rather than after the
// review matrix: the merge request IID is what posting needs, and nothing
// about it improves by spending a multi-agent review first.
func TestVerifyGitLabMRRange_MissingMRIIDFailsEarly(t *testing.T) {
	clearForgeCIEnv(t)
	t.Setenv("CI_PROJECT_PATH", "group/project")

	err := verifyMRRangeErr(
		context.Background(), ciReviewOpts{}, t.TempDir(), "base..head", false)
	require.ErrorContains(t, err, "CI_MERGE_REQUEST_IID")
	assert.Contains(t, err.Error(), "--pr")
}

// The canonical range replaces the expressions it was resolved from, so a
// revision like ":/fix" cannot name one commit while it is checked and another
// while it is reviewed.
func TestVerifyGitLabMRRange_ReturnsResolvedRange(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.HeadSHA()
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("one.txt", "one", "first")

	api := &stubGitLabMRAPI{headSHA: head, baseSHA: base}
	api.start(t)

	canonical, err := verifyGitLabMRRange(
		context.Background(), ciReviewOpts{pr: 7}, repo.Path(),
		base+"..feature", false)
	require.NoError(t, err)
	assert.Equal(t, base+".."+head, canonical,
		"the reviewed range must name commits, not the refs they came from")
}

// A head that moves between the initial validation and the pre-post recheck is
// the same benign race an auto-detected run reports, not the bad input a
// first-pass mismatch means, so the recheck reports it as such and the job
// exits without failing.
func TestVerifyGitLabMRRange_RecheckTreatsMovedHeadAsRace(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	base := repo.HeadSHA()
	repo.CheckoutNewBranch("feature")
	head := repo.CommitFile("one.txt", "one", "first")

	const movedTo = "feedface00000000000000000000000000000000"
	api := &stubGitLabMRAPI{headSHA: movedTo, baseSHA: base}
	api.start(t)

	opts := ciReviewOpts{pr: 7}
	gitRef := base + ".." + head

	err := verifyMRRangeErr(context.Background(), opts, repo.Path(), gitRef, false)
	require.Error(t, err)
	require.NotErrorIs(t, err, errMRHeadMoved,
		"a first-pass mismatch is bad input and must fail loudly")

	err = verifyMRRangeErr(context.Background(), opts, repo.Path(), gitRef, true)
	require.ErrorIs(t, err, errMRHeadMoved)
}

// The binding is re-checked immediately before posting, so a force push landing
// mid-review cannot leave a verdict describing code the merge request no longer
// has.
func TestPostGitLabCIComment_RefusesStaleHead(t *testing.T) {
	const mrHead = "feedface00000000000000000000000000000000"
	const reviewed = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	api := &stubGitLabMRAPI{headSHA: mrHead}
	api.start(t)

	err := postGitLabCIComment(
		context.Background(),
		ciReviewOpts{pr: 7}, "body", false, "", reviewed)
	require.Error(t, err)
	assert.False(t, api.posted,
		"no note may be posted once the head no longer matches")
}

// upsert_comments is the one [ci] setting with no flag, so in the protected
// worktree flow the merge request's own .roborev.toml decided whether an
// earlier note — findings and all — got replaced. The flag pins it either way.
func TestResolveCIUpsert(t *testing.T) {
	on, off := true, false
	repoOn := &config.RepoConfig{CI: config.RepoCIConfig{UpsertComments: &on}}

	tests := []struct {
		name string
		flag *bool
		repo *config.RepoConfig
		want bool
	}{
		{name: "RepoConfigAppliesWithoutFlag", repo: repoOn, want: true},
		{name: "FlagFalseBeatsRepoConfig", flag: &off, repo: repoOn, want: false},
		{name: "FlagTrueBeatsAbsentRepoConfig", flag: &on, want: true},
		{name: "DefaultsToAppendOnly", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveCIUpsert(
				ciReviewOpts{upsertComments: tt.flag}, tt.repo, nil))
		})
	}
}

// Global config is the operator's file, so it may not come from the tree under
// review. Any way of pointing ROBOREV_DATA_DIR (or HOME) into the checkout
// lands here: a relative path resolving against the working directory, a
// symlink such as /proc/self/cwd that is absolute but resolves inside it, or
// the checkout's own absolute path. Without this an author could commit a
// config.toml whose codex_cmd names a binary they also committed.
func TestLoadCIGlobalConfig(t *testing.T) {
	writeConfig := func(t *testing.T, dir string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "config.toml"),
			[]byte("codex_cmd = \"./payload\"\n"), 0o644))
	}

	t.Run("OutsideCheckoutIsLoaded", func(t *testing.T) {
		repo := testutil.NewTestRepoWithCommit(t)
		dataDir := t.TempDir()
		writeConfig(t, dataDir)
		t.Setenv("ROBOREV_DATA_DIR", dataDir)

		cfg := loadCIGlobalConfig(repo.Path())
		require.NotNil(t, cfg)
		assert.Equal(t, "./payload", cfg.CodexCmd)
	})

	t.Run("InsideCheckoutIsIgnored", func(t *testing.T) {
		repo := testutil.NewTestRepoWithCommit(t)
		writeConfig(t, filepath.Join(repo.Path(), "ci-data"))
		t.Setenv("ROBOREV_DATA_DIR", filepath.Join(repo.Path(), "ci-data"))

		assert.Nil(t, loadCIGlobalConfig(repo.Path()),
			"config committed to the reviewed tree must not be trusted")
	})

	t.Run("CheckoutRootIsIgnored", func(t *testing.T) {
		repo := testutil.NewTestRepoWithCommit(t)
		writeConfig(t, repo.Path())
		t.Setenv("ROBOREV_DATA_DIR", repo.Path())

		assert.Nil(t, loadCIGlobalConfig(repo.Path()))
	})

	// A symlink that is absolute but resolves into the checkout, which is what
	// /proc/self/cwd is on Linux.
	t.Run("SymlinkIntoCheckoutIsIgnored", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs privileges on Windows")
		}
		repo := testutil.NewTestRepoWithCommit(t)
		writeConfig(t, repo.Path())
		link := filepath.Join(t.TempDir(), "cwd")
		require.NoError(t, os.Symlink(repo.Path(), link))
		t.Setenv("ROBOREV_DATA_DIR", link)

		assert.Nil(t, loadCIGlobalConfig(repo.Path()),
			"an absolute path that resolves into the checkout is still the checkout")
	})
}
