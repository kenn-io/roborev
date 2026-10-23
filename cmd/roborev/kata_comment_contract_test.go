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

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

func TestKataCommentContractCommandDiscovery(t *testing.T) {
	root := newRootCmd()
	cmd, args, err := root.Find([]string{"kata", "comment"})
	require.NoError(t, err)
	assert.Empty(t, args)
	assert.Equal(t, "roborev kata comment", cmd.CommandPath())
	assert.NotNil(t, cmd.InheritedFlags().Lookup("from-skill"))
}

func TestKataCommentContractRejectsInvalidIntentLocally(t *testing.T) {
	selector, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"short confirm", []string{"--confirm", selector.String(), "-m", "too short"}, "at least 40"},
		{"short Unicode refute", []string{"--refute", selector.String(), "-m", strings.Repeat("界", 39)}, "at least 40"},
		{"supersede replacement", []string{"--supersede", selector.String(), "-m", "replacement evidence"}, "--replacement"},
		{"invalid target", []string{"--reply", "invalid", "-m", "evidence"}, "target"},
		{"invalid comment suffix", []string{"--reply", "c:iiiiii", "-m", "evidence"}, "target"},
		{"empty key", []string{"--reply", selector.String(), "--idempotency-key", "", "-m", "evidence"}, "--idempotency-key"},
		{"empty target", []string{"--reply", "", "-m", "evidence"}, "target"},
		{"invalid encoding", []string{"--reply", selector.String(), "-m", "\xff"}, "encoding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := kataCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(append([]string{"comment"}, tc.args...))
			require.ErrorContains(t, cmd.Execute(), tc.want)
			assert.NotContains(t, out.String(), "Idempotency key:")
		})
	}
}

func TestKataCommentContractPrintsKeyBeforeDiscoveryAndUsesSelector(t *testing.T) {
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			selector, err := findingid.New(uuid.New(), 0, strings.Repeat("a", 64))
			require.NoError(t, err)
			replacement, err := findingid.New(uuid.New(), 0, strings.Repeat("b", 64))
			require.NoError(t, err)
			evidence := strings.Repeat("界", 40)
			operation := "Fix-42:Attempt-A"
			var out bytes.Buffer
			var decodeErr error
			contacted := false
			probe := func(_ http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
				assert.Contains(t, out.String(), operation, "the retry key must precede daemon discovery")
				return false
			}
			NewMockDaemon(t, MockRefineHooks{OnStatus: probe, OnPing: probe, OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
				if r.URL.Path != "/api/kata/response" {
					return false
				}
				contacted = true
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Contains(t, out.String(), operation)
				var request kataresponse.Request
				decodeErr = json.NewDecoder(r.Body).Decode(&request)
				if decodeErr != nil {
					w.WriteHeader(http.StatusBadRequest)
					return true
				}
				assert.Zero(t, request.JobID, "the daemon resolves the selector's original job")
				assert.Equal(t, selector.String(), request.FindingID)
				assert.Equal(t, operation, request.OperationID)
				assert.Equal(t, kind, request.Kind)
				if kind == "supersede" {
					assert.Equal(t, replacement.String(), request.Replacement)
				} else {
					assert.Empty(t, request.Replacement)
				}
				assert.Equal(t, evidence, request.Body)
				w.WriteHeader(http.StatusAccepted)
				assert.NoError(t, json.NewEncoder(w).Encode(storage.KataResponseStatus{OperationID: operation, State: "pending", Kind: kind}))
				return true
			}})
			cmd := kataCmd()
			cmd.SetOut(&out)
			args := []string{"comment", "--" + kind, selector.String(), "--idempotency-key", operation, "-m", evidence}
			if kind == "supersede" {
				args = append(args, "--replacement", replacement.String())
			}
			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute())
			require.NoError(t, decodeErr)
			assert.True(t, contacted)
		})
	}
}

func TestKataShowDisplaysPendingResponseKey(t *testing.T) {
	NewMockDaemon(t, MockRefineHooks{OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
		if r.URL.Path != "/api/kata/publications" {
			return false
		}
		assert.Equal(t, "42", r.URL.Query().Get("job_id"))
		assert.NoError(t, json.NewEncoder(w).Encode(daemon.KataPublicationsBody{JobID: 42, Publications: []daemon.KataPublication{}, Operations: []storage.KataResponseStatus{{OperationID: "Fix-42:Attempt-A", State: "pending", Kind: "refute", ExistingComment: "c:abcdef", Message: "A duplicate reply remains unresolved."}}}))
		return true
	}})
	cmd := kataCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "--job", "42"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Fix-42:Attempt-A")
	assert.Contains(t, out.String(), "pending (refute)")
	assert.Contains(t, out.String(), "Existing comment: c:abcdef")
	assert.Contains(t, out.String(), "A duplicate reply remains unresolved.")
}
