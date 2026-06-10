package kata_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

func TestResolveCurrentNormalizesRefs(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		Issues:        map[string]kata.Issue{"abc4": {ShortID: "abc4", Title: "Task", Body: "Body"}},
	}
	res := kata.ResolveContext(context.Background(), f, "current",
		[]string{"Implement\n\nCloses: kata#abc4", "follow-up roborev#abc4"})

	require.Len(t, res.Issues, 1)
	assert.Equal(t, "Task", res.Issues[0].Title)
	assert.Empty(t, res.Notes)
	assert.Equal(t, []string{"abc4"}, f.ShowRefs) // not "kata#abc4", and deduped
}

func TestResolveCurrentLoadFailureNote(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ShowErr:       map[string]error{"abc4": errors.New("not found")},
	}
	res := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	assert.Empty(t, res.Issues)
	require.Len(t, res.Notes, 1)
	assert.Contains(t, res.Notes[0], "roborev#abc4")
	require.Len(t, res.Errs, 1, "load failure must surface for logging")
	assert.Contains(t, res.Errs[0].Error(), "not found")
}

func TestResolveBrokenBindingSurfacesError(t *testing.T) {
	f := &katatest.FakeClient{BindingErr: errors.New("kata: parse .kata.toml: bad toml")}
	res := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	assert.Empty(t, res.Issues)
	require.Len(t, res.Errs, 1, "a broken .kata.toml must not silently disable context")
	assert.Contains(t, res.Errs[0].Error(), "bad toml")
}

func TestResolveOpenListFailureSurfacesError(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListErr:       errors.New("kata list: exit 1: boom"),
	}
	res := kata.ResolveContext(context.Background(), f, "open", nil)
	assert.Empty(t, res.Issues)
	require.Len(t, res.Errs, 1, "a failing list must not silently disable context")
	assert.Contains(t, res.Errs[0].Error(), "boom")
}

func TestResolveOpenUnavailableIsInert(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListErr:       kata.ErrUnavailable,
	}
	res := kata.ResolveContext(context.Background(), f, "open", nil)
	assert.Empty(t, res.Issues)
	assert.Empty(t, res.Errs)
}

func TestResolveCurrentUnavailableIsInert(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ShowErr:       map[string]error{"abc4": kata.ErrUnavailable},
	}
	res := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	assert.Empty(t, res.Issues)
	assert.Empty(t, res.Notes)
}

func TestResolveNoBindingInert(t *testing.T) {
	f := &katatest.FakeClient{BindingErr: kata.ErrNoBinding}
	res := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	assert.Empty(t, res.Issues)
	assert.Empty(t, res.Notes)
}

func TestResolveOpenListsAll(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListResult:    []kata.Issue{{ShortID: "a"}, {ShortID: "b"}},
	}
	res := kata.ResolveContext(context.Background(), f, "open", nil)
	assert.Len(t, res.Issues, 2)
	require.Len(t, f.ListOpts, 1)
	assert.Equal(t, "open", f.ListOpts[0].Status)
}

func TestResolveOpenExcludesRoborevFiledIssues(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListResult: []kata.Issue{
			{ShortID: "a", Title: "Real task"},
			{ShortID: "b", Title: "Review findings for ...", Labels: []string{kata.RoborevLabel, "review-finding"}},
			{ShortID: "c", Title: "Other task", Labels: []string{"feature"}},
		},
	}
	res := kata.ResolveContext(context.Background(), f, "open", nil)
	require.Len(t, res.Issues, 2,
		"issues filed by the roborev hook must not feed back into review prompts")
	assert.Equal(t, "a", res.Issues[0].ShortID)
	assert.Equal(t, "c", res.Issues[1].ShortID)
}

func TestResolveNilClient(t *testing.T) {
	res := kata.ResolveContext(context.Background(), nil, "current", []string{"kata#abc4"})
	assert.Empty(t, res.Issues)
}
