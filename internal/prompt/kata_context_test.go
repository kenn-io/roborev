package prompt

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
	"go.kenn.io/roborev/internal/testutil"
)

func TestTrimNextDropsGuidelinesBeforeKata(t *testing.T) {
	o := &ReviewOptionalContext{
		ProjectGuidelines: &MarkdownSection{Heading: "## G", Body: "guide"},
		KataContext:       &MarkdownSection{Heading: "## K", Body: "task"},
	}
	require.True(t, o.TrimNext())
	assert.Nil(t, o.ProjectGuidelines, "guidelines should drop first")
	assert.NotNil(t, o.KataContext, "kata context preserved longer")
	require.True(t, o.TrimNext())
	assert.Nil(t, o.KataContext, "kata context dropped last")
	assert.False(t, o.TrimNext())
}

func TestBuildKataContextSectionView(t *testing.T) {
	v := buildKataContextSectionView(
		[]kata.Issue{{QualifiedID: "p#abc4", Title: "Add widget", Body: "Build the widget.", Status: "open"}},
		[]string{"Referenced p#def5 could not be loaded."}, 50000, config.KataModeCurrent)
	require.NotNil(t, v)
	assert.Contains(t, v.Body, "p#abc4")
	assert.Contains(t, v.Body, "Add widget")
	assert.Contains(t, v.Body, "Build the widget.")
	assert.Contains(t, v.Body, "could not be loaded")
	assert.Contains(t, v.Body, "authoritative task description",
		"current-mode issues are explicitly referenced and must be framed as intent")
}

func TestBuildKataContextSectionViewOpenModeIntro(t *testing.T) {
	v := buildKataContextSectionView(
		[]kata.Issue{{QualifiedID: "p#abc4", Title: "Add widget", Body: "Build the widget.", Status: "open"}},
		nil, 50000, config.KataModeOpen)
	require.NotNil(t, v)
	assert.Contains(t, v.Body, "background context",
		"open-mode issues are the whole backlog and must not be framed as authoritative")
	assert.NotContains(t, v.Body, "authoritative task description")
}

func TestBuildKataContextSectionViewTruncates(t *testing.T) {
	big := make([]byte, 200)
	for i := range big {
		big[i] = 'x'
	}
	v := buildKataContextSectionView([]kata.Issue{{ShortID: "a", Title: "T", Body: string(big)}}, nil, 80, config.KataModeCurrent)
	require.NotNil(t, v)
	maxLen := len(kataIntroCurrent) + len("\n\n") + 80 + len("\n\n_[kata context truncated]_")
	assert.LessOrEqual(t, len(v.Body), maxLen, "truncation budget applies to the issue body, not the fixed intro")
	assert.Contains(t, v.Body, "_[kata context truncated]_")
}

func TestBuildKataContextSectionViewEmpty(t *testing.T) {
	assert.Nil(t, buildKataContextSectionView(nil, nil, 50000, config.KataModeCurrent))
}

func TestBuilderInjectsKataContext(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.CommitFile("feature.txt", "new feature\n", "Implement feature\n\nCloses: kata#abc4")

	fake := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		Issues: map[string]kata.Issue{
			"abc4": {ShortID: "abc4", QualifiedID: "roborev#abc4", Title: "Build widget", Body: "Widget spec here.", Status: "open"},
		},
	}
	g := &config.Config{}
	g.KataContext.Mode = config.KataModeCurrent

	b := NewBuilderWithConfig(nil, g).ForRepo(repo.Path(), 0).WithKataClient(fake)
	out, err := b.Build(sha, 0, "test", "", "")
	require.NoError(t, err)
	assert.Contains(t, out, "Task Context (kata)")
	assert.Contains(t, out, "Build widget")
	assert.Contains(t, out, "Widget spec here.")
	assert.Equal(t, []string{"abc4"}, fake.ShowRefs)
}

func TestBuildDirtyInjectsOpenKataContext(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)

	fake := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListResult: []kata.Issue{
			{ShortID: "abc4", QualifiedID: "roborev#abc4", Title: "Build widget", Body: "Widget spec here.", Status: "open"},
		},
	}
	g := &config.Config{}
	g.KataContext.Mode = config.KataModeOpen

	b := NewBuilderWithConfig(nil, g).ForRepo(repo.Path(), 0).WithKataClient(fake)
	out, err := b.BuildDirty("diff --git a/f.txt b/f.txt\n+dirty\n", 0, "test", "", "")
	require.NoError(t, err)
	assert.Contains(t, out, "Task Context (kata)")
	assert.Contains(t, out, "Build widget")
	assert.Contains(t, out, "Widget spec here.")
	assert.Contains(t, out, "background context")
	require.Len(t, fake.ListOpts, 1)
	assert.Equal(t, "open", fake.ListOpts[0].Status)
}

func TestBuildDirtyNoKataContextInCurrentMode(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	fake := &katatest.FakeClient{BindingResult: kata.Binding{Project: "roborev"}}
	g := &config.Config{}
	g.KataContext.Mode = config.KataModeCurrent

	b := NewBuilderWithConfig(nil, g).ForRepo(repo.Path(), 0).WithKataClient(fake)
	out, err := b.BuildDirty("diff --git a/f.txt b/f.txt\n+dirty\n", 0, "test", "", "")
	require.NoError(t, err)
	assert.NotContains(t, out, "Task Context (kata)")
	assert.Empty(t, fake.ShowRefs, "no commit messages means no refs to resolve")
}

func TestBuilderNoKataContextWhenModeOff(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.CommitFile("f.txt", "x\n", "Implement\n\nCloses: kata#abc4")
	fake := &katatest.FakeClient{BindingResult: kata.Binding{Project: "roborev"}}

	b := NewBuilderWithConfig(nil, &config.Config{}).ForRepo(repo.Path(), 0).WithKataClient(fake)
	out, err := b.Build(sha, 0, "test", "", "")
	require.NoError(t, err)
	assert.NotContains(t, out, "Task Context (kata)")
	assert.Empty(t, fake.ShowRefs, "off mode must not query kata")
}

func TestBuildDirtyAbortsWhenKataResolutionCanceled(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	fake := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListErr:       fmt.Errorf("kata list: %w", context.Canceled),
	}
	g := &config.Config{}
	g.KataContext.Mode = config.KataModeOpen

	b := NewBuilderWithConfig(nil, g).ForRepo(repo.Path(), 0).WithKataClient(fake)
	_, err := b.BuildDirty("diff --git a/f.txt b/f.txt\n+dirty\n", 0, "test", "", "")
	require.ErrorIs(t, err, context.Canceled,
		"a canceled kata resolution must abort the dirty prompt build, not degrade to a kata-less prompt")
}
