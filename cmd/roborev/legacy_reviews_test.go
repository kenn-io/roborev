package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestLegacyReviewCommands(t *testing.T) {
	archiveID := uuid.New()
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/maintenance/legacy/export":
			var target daemon.LegacyMaintenanceTarget
			assert.NoError(t, json.UnmarshalRead(r.Body, &target))
			assert.Empty(t, target.DB)
			assert.Empty(t, target.PostgresURL)
			assert.NoError(t, json.MarshalWrite(w, map[string]any{"sqlite_records": []storage.LegacyReview{{ID: 7, Output: "Archived prose"}}, "postgres_records": []storage.PostgresLegacyReview{}}))
		case "/api/maintenance/legacy/convert":
			var request daemon.ConvertLegacyReviewsInput
			assert.NoError(t, json.UnmarshalRead(r.Body, &request.Body))
			assert.True(t, request.Body.DryRun)
			assert.Equal(t, dbPath, request.Body.DB)
			assert.NoError(t, json.MarshalWrite(w, storage.LegacyConversionReport{Unresolved: 2, Converted: 1, Refused: map[string]int{"unrecognized_format": 1}, DryRun: true}))
		case "/api/maintenance/legacy/import":
			var request daemon.ImportLegacyReviewInput
			assert.NoError(t, json.UnmarshalRead(r.Body, &request.Body))
			assert.Equal(t, "postgres://archive.example.test/reviews", request.Body.PostgresURL)
			assert.Equal(t, archiveID, request.Body.UUID)
			assert.JSONEq(t, string(testutil.ReviewFixtureJSON("Converted review.")), string(request.Body.Document))
			w.WriteHeader(http.StatusNoContent)
		default:
			assert.Equal(t, "/api/maintenance/legacy/import", r.URL.Path, "unexpected maintenance endpoint")
		}
	}))
	var out bytes.Buffer
	command := legacyReviewsCmd()
	command.SetOut(&out)
	command.SetArgs([]string{"export"})
	require.NoError(t, command.Execute())
	var exported struct {
		Records []storage.LegacyReview `json:"records"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &exported))
	require.Len(t, exported.Records, 1)
	assert.Equal(t, int64(7), exported.Records[0].ID)
	assert.Equal(t, "Archived prose", exported.Records[0].Output)

	out.Reset()
	command = legacyReviewsCmd()
	command.SetOut(&out)
	command.SetArgs([]string{"--db", dbPath, "convert", "--dry-run"})
	require.NoError(t, command.Execute())
	assert.Equal(t, "Historical reviews needing conversion: 2\nWould convert: 1\nWould remain unstructured: 1\n  unrecognized_format: 1\n", out.String())

	out.Reset()
	command = legacyReviewsCmd()
	command.SetIn(bytes.NewReader(testutil.ReviewFixtureJSON("Converted review.")))
	command.SetOut(&out)
	command.SetArgs([]string{"--postgres-url", "postgres://archive.example.test/reviews", "import", archiveID.String()})
	require.NoError(t, command.Execute())
	assert.Equal(t, "Converted review restored. The original remains archived.\n", out.String())
}

func TestLegacyReviewsReportsDaemonFailure(t *testing.T) {
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"detail":"use --server to select the daemon that owns the database"}`))
		assert.NoError(t, err)
	}))
	command := legacyReviewsCmd()
	command.SetArgs([]string{"--db", filepath.Join(t.TempDir(), "other.db"), "convert"})
	err := command.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "use --server")
}

func TestLegacyReviewsResolvesDatabaseInCallerDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var target daemon.LegacyMaintenanceTarget
		assert.NoError(t, json.UnmarshalRead(r.Body, &target))
		assert.Equal(t, filepath.Join(dir, "reviews.db"), target.DB)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"sqlite_records":[],"postgres_records":[]}`))
		assert.NoError(t, err)
	}))
	command := legacyReviewsCmd()
	command.SetOut(&bytes.Buffer{})
	command.SetArgs([]string{"--db", "reviews.db", "export"})
	require.NoError(t, command.Execute())
}

func TestLegacyReviewsExportSchemasAcceptFaithfulConversions(t *testing.T) {
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"sqlite_records":[],"postgres_records":[]}`))
		assert.NoError(t, err)
	}))
	var out bytes.Buffer
	command := legacyReviewsCmd()
	command.SetOut(&out)
	command.SetArgs([]string{"export"})
	require.NoError(t, command.Execute())
	var exported struct {
		Schema          jsonschema.Schema `json:"schema"`
		SynthesisSchema jsonschema.Schema `json:"synthesis_schema"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &exported))

	// A historical review states no verdict, kind, or confidence, and gives
	// its location as free text. Converting it must not invent them.
	finding := `{"severity":"medium","kind":null,"topic":null,"confidence":null,"problem":"Retries never stop.","fix":"Cap the retries.","file":null,"start_line":null,"end_line":null,"location":"the retry loop in worker.go"%s}`
	single := fmt.Sprintf(`{"schema_version":3,"summary":"Historical review.","verdict":null,"findings":[`+finding+`]}`, "")
	sourced := fmt.Sprintf(`{"schema_version":3,"summary":"Historical review.","verdict":null,"findings":[`+finding+`]}`, `,"sources":[1]`)
	for _, tc := range []struct {
		name     string
		schema   *jsonschema.Schema
		document string
	}{
		{"single", &exported.Schema, single},
		{"synthesis", &exported.SynthesisSchema, sourced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator, err := tc.schema.Resolve(nil)
			require.NoError(t, err)
			var value any
			require.NoError(t, json.Unmarshal([]byte(tc.document), &value))
			require.NoError(t, validator.Validate(value))

			doc, err := structuredreview.Decode(jsontext.Value(tc.document))
			require.NoError(t, err)
			require.Len(t, doc.Findings, 1)
			assert.Empty(t, doc.Findings[0].Kind)
			assert.Equal(t, "the retry loop in worker.go", doc.Findings[0].Location)
		})
	}

	// Live reviewers must still state a kind and confidence. The two
	// documents differ only in those fields.
	var live jsonschema.Schema
	require.NoError(t, json.Unmarshal(structuredreview.Schema, &live))
	validator, err := live.Resolve(nil)
	require.NoError(t, err)
	liveDoc := `{"schema_version":3,"summary":"Live review.","verdict":"fail","findings":[{"severity":"medium","kind":%s,"topic":null,"confidence":%s,"problem":"Retries never stop.","fix":"Cap the retries.","file":null,"start_line":null,"end_line":null}]}`
	liveValue := func(kind, confidence string) any {
		var value any
		require.NoError(t, json.Unmarshal(fmt.Appendf(nil, liveDoc, kind, confidence), &value))
		return value
	}
	require.NoError(t, validator.Validate(liveValue(`"defect"`, "0.8")))
	require.Error(t, validator.Validate(liveValue("null", "null")))
}
