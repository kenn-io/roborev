package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
)

func TestDoctorKataRubrics(t *testing.T) {
	cases := []struct {
		name, mode                    string
		repo                          *config.RepoConfig
		cliErr, bindingErr, healthErr error
		health                        kata.DocbankHealth
		want                          doctorStatus
		summary                       string
	}{
		{name: "off", mode: "off", want: doctorInfo, summary: "off"},
		{name: "repository off", mode: "current", repo: &config.RepoConfig{KataRubrics: config.KataRubricsConfig{Mode: "off"}}, want: doctorInfo, summary: "off"},
		{name: "key resolves", mode: "current", health: kata.DocbankHealth{Credential: "ok"}, want: doctorOK, summary: "key resolves"},
		{name: "missing kata", mode: "current", cliErr: errors.New("missing"), want: doctorFail, summary: "kata"},
		{name: "missing binding", mode: "current", bindingErr: kata.ErrNoBinding, want: doctorFail, summary: "binding"},
		{name: "docbank credential missing", mode: "current", health: kata.DocbankHealth{Credential: "missing"}, want: doctorFail, summary: "Docbank"},
		{name: "old kata", mode: "current", want: doctorInfo, summary: "unknown"},
		{name: "health unavailable", mode: "current", healthErr: errors.New("unavailable"), want: doctorInfo, summary: "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &doctorEnv{ctx: t.Context(), repoPath: t.TempDir(), global: &config.Config{KataRubrics: config.KataRubricsConfig{Mode: tc.mode}}, repoCfg: tc.repo}
			env.rubricKata = func(context.Context, string) (kata.Binding, kata.DocbankHealth, error, error, error) {
				return kata.Binding{Project: "example"}, tc.health, tc.cliErr, tc.bindingErr, tc.healthErr
			}
			got := checkDoctorKataRubrics(env)[0]
			assert.Equal(t, tc.want, got.Status)
			assert.Contains(t, got.Summary, tc.summary)
		})
	}
}
