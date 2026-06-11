package kata_test

import (
	"context"
	"errors"
	"fmt"
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
	res, rerr := kata.ResolveContext(context.Background(), f, "current",
		[]string{"Implement\n\nCloses: kata#abc4", "follow-up roborev#abc4"})
	require.NoError(t, rerr)

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
	res, rerr := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
	require.Len(t, res.Notes, 1)
	assert.Contains(t, res.Notes[0], "roborev#abc4")
	require.Len(t, res.Errs, 1, "load failure must surface for logging")
	assert.Contains(t, res.Errs[0].Error(), "not found")
}

func TestResolveBrokenBindingSurfacesError(t *testing.T) {
	f := &katatest.FakeClient{BindingErr: errors.New("kata: parse .kata.toml: bad toml")}
	res, rerr := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
	require.Len(t, res.Errs, 1, "a broken .kata.toml must not silently disable context")
	assert.Contains(t, res.Errs[0].Error(), "bad toml")
}

func TestResolveOpenListFailureSurfacesError(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListErr:       errors.New("kata list: exit 1: boom"),
	}
	res, rerr := kata.ResolveContext(context.Background(), f, "open", nil)
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
	require.Len(t, res.Errs, 1, "a failing list must not silently disable context")
	assert.Contains(t, res.Errs[0].Error(), "boom")
}

func TestResolveOpenUnavailableIsInert(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListErr:       kata.ErrUnavailable,
	}
	res, rerr := kata.ResolveContext(context.Background(), f, "open", nil)
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
	assert.Empty(t, res.Errs)
}

func TestResolveCurrentUnavailableIsInert(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ShowErr:       map[string]error{"abc4": kata.ErrUnavailable},
	}
	res, rerr := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
	assert.Empty(t, res.Notes)
}

func TestResolveNoBindingInert(t *testing.T) {
	f := &katatest.FakeClient{BindingErr: kata.ErrNoBinding}
	res, rerr := kata.ResolveContext(context.Background(), f, "current", []string{"kata#abc4"})
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
	assert.Empty(t, res.Notes)
}

func TestResolveOpenListsAll(t *testing.T) {
	f := &katatest.FakeClient{
		BindingResult: kata.Binding{Project: "roborev"},
		ListResult:    []kata.Issue{{ShortID: "a"}, {ShortID: "b"}},
	}
	res, rerr := kata.ResolveContext(context.Background(), f, "open", nil)
	require.NoError(t, rerr)
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
	res, rerr := kata.ResolveContext(context.Background(), f, "open", nil)
	require.NoError(t, rerr)
	require.Len(t, res.Issues, 2,
		"issues filed by the roborev hook must not feed back into review prompts")
	assert.Equal(t, "a", res.Issues[0].ShortID)
	assert.Equal(t, "c", res.Issues[1].ShortID)
}

func TestResolveNilClient(t *testing.T) {
	res, rerr := kata.ResolveContext(context.Background(), nil, "current", []string{"kata#abc4"})
	require.NoError(t, rerr)
	assert.Empty(t, res.Issues)
}

func TestResolveContextPropagatesCancellation(t *testing.T) {
	tests := []struct {
		name    string
		fake    *katatest.FakeClient
		mode    string
		wantErr error
	}{
		{"binding canceled", &katatest.FakeClient{BindingErr: fmt.Errorf("kata: %w", context.Canceled)}, "open", context.Canceled},
		{"list canceled", &katatest.FakeClient{
			BindingResult: kata.Binding{Project: "p"},
			ListErr:       fmt.Errorf("kata list: %w", context.Canceled),
		}, "open", context.Canceled},
		{"show deadline exceeded", &katatest.FakeClient{
			BindingResult: kata.Binding{Project: "p"},
			ShowErr:       map[string]error{"abc4": fmt.Errorf("kata show: %w", context.DeadlineExceeded)},
		}, "current", context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := kata.ResolveContext(context.Background(), tt.fake, tt.mode, []string{"kata#abc4"})
			require.ErrorIs(t, err, tt.wantErr,
				"cancellation must propagate with its original kind, not degrade to an empty result")
		})
	}
}

func TestResolveContextPreCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &katatest.FakeClient{BindingResult: kata.Binding{Project: "p"}}
	_, err := kata.ResolveContext(ctx, f, "open", nil)
	require.ErrorIs(t, err, context.Canceled)
}
