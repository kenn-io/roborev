package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataCommentPrintsOperationBeforeNetwork(t *testing.T) {
	id, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
	require.NoError(t, err)
	var out bytes.Buffer
	contacted := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/kata/response" {
			return
		}
		contacted = true
		assert.Contains(t, out.String(), "Idempotency key:", "the recovery ID must be visible before the first response request")
		var request kataresponse.Request
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Contains(t, out.String(), request.OperationID)
		assert.NoError(t, kataresponse.ValidateOperationID(request.OperationID), "an omitted key generates a UUID")
		w.WriteHeader(http.StatusAccepted)
		assert.NoError(t, json.NewEncoder(w).Encode(storage.KataResponseStatus{OperationID: request.OperationID, State: "pending", Kind: request.Kind}))
	})
	probe := func(_ http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
		assert.Contains(t, out.String(), "Idempotency key:", "operation must be printed before daemon discovery probes")
		return false
	}
	NewMockDaemon(t, MockRefineHooks{OnStatus: probe, OnPing: probe, OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		handler.ServeHTTP(w, r)
		return true
	}})
	cmd := kataCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"comment", "--reply", id.String(), "-m", "Evidence in the committed fix."})
	require.NoError(t, cmd.Execute())
	assert.True(t, contacted)
}

func TestKataCommentReportsOldDaemon(t *testing.T) {
	id, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
	require.NoError(t, err)
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"comment", "--reply", id.String(), "-m", "Evidence"})
	require.ErrorContains(t, cmd.Execute(), "does not support typed responses")
	assert.Contains(t, out.String(), "Idempotency key:")
}

func TestKataCommentKeepsNewDaemonMissingFindingDiagnostic(t *testing.T) {
	id, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
	require.NoError(t, err)
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"detail": "finding snapshot unavailable", "status": 404}))
		return true
	}})
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"comment", "--reply", id.String(), "-m", "Evidence"})
	require.ErrorContains(t, cmd.Execute(), "finding snapshot unavailable")
}

func TestKataCommentReasonBeforeNetwork(t *testing.T) {
	for _, reason := range []string{"false-positive", "spurious", "risk-accepted"} {
		t.Run(reason, func(t *testing.T) {
			var out bytes.Buffer
			contacted := false
			NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				if r.URL.Path != "/api/kata/response" {
					return false
				}
				contacted = true
				assert.Contains(t, out.String(), "--reason '"+reason+"'")
				var request kataresponse.Request
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.NotNil(t, request.Reason)
				assert.Equal(t, reason, *request.Reason)
				assert.Equal(t, strings.Repeat("界", 40), request.Body)
				w.WriteHeader(http.StatusAccepted)
				require.NoError(t, json.NewEncoder(w).Encode(storage.KataResponseStatus{OperationID: request.OperationID, State: "pending", Kind: request.Kind}))
				return true
			}})
			cmd := kataCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"comment", "--refute", "c:abcdef", "--reason", reason, "-m", strings.Repeat("界", 40)})
			require.NoError(t, cmd.Execute())
			assert.True(t, contacted)
		})
	}
}

func TestKataCommentRejectsReasonLocally(t *testing.T) {
	for _, args := range [][]string{
		{"--refute", "c:abcdef", "--reason", ""}, {"--refute", "c:abcdef", "--reason", "unknown"},
		{"--reply", "c:abcdef", "--reason", "spurious"}, {"--confirm", "c:abcdef", "--reason", "spurious"},
		{"--supersede", "c:abcdef", "--reason", "spurious"},
		{"--refute", "c:abcdef", "--reason", "spurious", "-m", strings.Repeat("界", 39)},
		{"--refute", "c:abcdef", "-m", "<!-- roborev-refute:v1 reason=spurious -->\n" + strings.Repeat("界", 40)},
	} {
		cmd := kataCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"comment", "-m", strings.Repeat("界", 40)}, args...))
		require.Error(t, cmd.Execute())
		assert.NotContains(t, out.String(), "Idempotency key:")
	}
}
