// Package katatest provides a fake kata.Client for tests.
package katatest

import (
	"context"
	"errors"

	"go.kenn.io/roborev/internal/kata"
)

// FakeClient is a configurable in-memory kata.Client.
type FakeClient struct {
	ArchiveErr    error
	Archives      map[string]map[string][]byte
	BindingResult kata.Binding
	BindingErr    error

	Issues  map[string]kata.Issue // keyed by ref, for Show
	ShowErr map[string]error      // keyed by ref

	ListResult []kata.Issue
	ListErr    error

	CreateResult kata.CreateResult
	CreateErr    error

	// Recorded calls.
	ShowRefs   []string
	ListOpts   []kata.ListOpts
	CreateReqs []kata.CreateReq
}

func (f *FakeClient) Binding(context.Context) (kata.Binding, error) {
	return f.BindingResult, f.BindingErr
}

func (f *FakeClient) List(_ context.Context, opts kata.ListOpts) ([]kata.Issue, error) {
	f.ListOpts = append(f.ListOpts, opts)
	return f.ListResult, f.ListErr
}

func (f *FakeClient) Show(_ context.Context, ref string) (kata.Issue, error) {
	f.ShowRefs = append(f.ShowRefs, ref)
	if f.ShowErr != nil {
		if err := f.ShowErr[ref]; err != nil {
			return kata.Issue{}, err
		}
	}
	if iss, ok := f.Issues[ref]; ok {
		return iss, nil
	}
	return kata.Issue{}, errors.New("not found")
}

func (f *FakeClient) Create(_ context.Context, req kata.CreateReq) (kata.CreateResult, error) {
	f.CreateReqs = append(f.CreateReqs, req)
	return f.CreateResult, f.CreateErr
}

// ArchiveRubric records a rubric once per issue and name.
func (f *FakeClient) ArchiveRubric(_ context.Context, _, ref, name string, data []byte) error {
	if f.ArchiveErr != nil {
		return f.ArchiveErr
	}
	if f.Archives == nil {
		f.Archives = make(map[string]map[string][]byte)
	}
	if f.Archives[ref] == nil {
		f.Archives[ref] = make(map[string][]byte)
	}
	if _, ok := f.Archives[ref][name]; !ok {
		f.Archives[ref][name] = append([]byte(nil), data...)
	}
	return nil
}
