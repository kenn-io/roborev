package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackfillTokensUsesDaemon(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "dry run"}[dryRun], func(t *testing.T) {
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/maintenance/tokens/backfill", r.URL.Path)
				assert.Equal(t, http.MethodPost, r.Method)
				var request struct {
					DryRun bool `json:"dry_run"`
				}
				assert.NoError(t, json.UnmarshalRead(r.Body, &request))
				assert.Equal(t, dryRun, request.DryRun)
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"total":2,"updated":1,"skipped":1,"failed":0,"jobs":[{"job_id":7,"agent":"codex","summary":"1,000 tokens"}]}`))
				assert.NoError(t, err)
			}))
			var out bytes.Buffer
			command := backfillTokensCmd()
			command.SetOut(&out)
			command.SetArgs([]string{})
			if dryRun {
				command.SetArgs([]string{"--dry-run"})
			}
			require.NoError(t, command.Execute())
			action := "Updated"
			if dryRun {
				action = "Would update"
			}
			assert.Equal(t, "job 7 (codex): 1,000 tokens\n\n"+action+" 1/2 jobs (1 skipped, 0 failed)\n", out.String())
		})
	}
}

func TestBackfillVerdictsUsesDaemon(t *testing.T) {
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/maintenance/verdicts/backfill", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"count":3}`))
		assert.NoError(t, err)
	}))
	var out bytes.Buffer
	command := backfillVerdictsCmd()
	command.SetOut(&out)
	command.SetArgs([]string{})
	require.NoError(t, command.Execute())
	assert.Equal(t, "Backfilled verdict_bool for 3 reviews.\n", out.String())
}

func TestBackfillVerdictsReportsDaemonProblemDetail(t *testing.T) {
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, err := w.Write([]byte(`{"status":500,"title":"Internal Server Error","detail":"could not repair review verdicts"}`))
		assert.NoError(t, err)
	}))
	command := backfillVerdictsCmd()
	command.SetArgs([]string{})
	err := command.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "could not repair review verdicts")
}
