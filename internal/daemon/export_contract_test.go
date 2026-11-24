package daemon

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// Frozen v2 export contract keys. Within schema_version 2, wire evolution is
// strictly additive: fields may be added, never removed, renamed, or changed in
// type or nullability. Breaking changes require a new schema_version while
// continuing to serve v2 when requested via schema_version=2.
var (
	exportReviewsV2HeaderKeys = []string{
		"schema_version",
		"tool",
		"tool_version",
		"generated_at",
		"database_id",
		"profile",
		"window",
		"truncated",
		"next_cursor",
		"reviews",
	}
	exportReviewsV2WindowKeys = []string{
		"field",
		"since",
		"until",
	}
	exportReviewsV2ReviewKeys = []string{
		"review_id",
		"source_machine_id",
		"exported_by_machine_id",
		"status",
		"verdict",
		"created_at",
		"completed_at",
		"duration_ms",
		"project",
		"repo",
		"branch",
		"commit_sha",
		"pr_number",
		"pr_url",
		"agent",
		"model",
		"cost",
		"content",
		"document",
		"closed",
		"updated_at",
		"subagents",
		"experiments",
		"resume_source_job_uuid",
	}
	exportReviewsV2CostKeys = []string{
		"tokens_in",
		"tokens_out",
		"usd",
	}
	exportReviewsV2SubagentKeys = []string{
		"review_id",
		"name",
		"non_voting",
		"agent",
		"model",
		"review_type",
		"verdict",
		"completed_at",
		"duration_ms",
		"cost",
		"content",
		"document",
		"resume_source_job_uuid",
	}
	exportReviewsV2ExperimentKeys = []string{
		"id",
		"arm",
		"subject_hash",
		"definition_hash",
		"effective_config_hash",
	}
	exportReviewsV2DocumentKeys = []string{
		"schema_version",
		"summary",
		"findings",
	}
	exportReviewsV2FindingKeys = []string{
		"severity",
		"kind",
		"topic",
		"confidence",
		"problem",
		"fix",
		"file",
		"start_line",
		"end_line",
		"location",
	}
	exportReviewsV2LegacyDocumentKeys = []string{
		"schema_version",
		"legacy",
	}
	exportReviewsV2LegacyPayloadKeys = []string{
		"markdown",
		"recorded_verdict",
	}
)

func requireJSONKey(t *testing.T, obj map[string]jsontext.Value, key string) jsontext.Value {
	t.Helper()
	raw, ok := obj[key]
	require.True(t, ok, "v2 contract object missing required key %q", key)
	return raw
}

func assertJSONNull(t *testing.T, obj map[string]jsontext.Value, key string) {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	assert.Equal(t, jsontext.Kind('n'), raw.Kind(), "key %q must be explicit JSON null, got %s", key, raw)
}

func assertJSONString(t *testing.T, obj map[string]jsontext.Value, key string) string {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	require.Equal(t, jsontext.Kind('"'), raw.Kind(), "key %q must be JSON string, got %s", key, raw)
	var val string
	require.NoError(t, json.Unmarshal(raw, &val))
	return val
}

func assertJSONInt64(t *testing.T, obj map[string]jsontext.Value, key string) int64 {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	require.Equal(t, jsontext.Kind('0'), raw.Kind(), "key %q must be JSON number, got %s", key, raw)
	var val int64
	require.NoError(t, json.Unmarshal(raw, &val))
	return val
}

func assertJSONFloat64(t *testing.T, obj map[string]jsontext.Value, key string) float64 {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	require.Equal(t, jsontext.Kind('0'), raw.Kind(), "key %q must be JSON number, got %s", key, raw)
	var val float64
	require.NoError(t, json.Unmarshal(raw, &val))
	return val
}

func assertJSONBool(t *testing.T, obj map[string]jsontext.Value, key string) bool {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	kind := raw.Kind()
	require.Contains(t, []jsontext.Kind{'t', 'f'}, kind, "key %q must be JSON boolean, got %s", key, raw)
	var val bool
	require.NoError(t, json.Unmarshal(raw, &val))
	return val
}

func assertJSONObject(t *testing.T, obj map[string]jsontext.Value, key string) map[string]jsontext.Value {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	require.Equal(t, jsontext.Kind('{'), raw.Kind(), "key %q must be JSON object, got %s", key, raw)
	var nested map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &nested))
	return nested
}

func assertJSONArray(t *testing.T, obj map[string]jsontext.Value, key string) []map[string]jsontext.Value {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	require.Equal(t, jsontext.Kind('['), raw.Kind(), "key %q must be JSON array (never null), got %s", key, raw)
	var items []map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &items))
	return items
}

func assertRFC3339String(t *testing.T, obj map[string]jsontext.Value, key string) {
	t.Helper()
	val := assertJSONString(t, obj, key)
	_, err := time.Parse(time.RFC3339, val)
	require.NoError(t, err, "key %q must be RFC3339 timestamp, got %q", key, val)
}

func assertJSONUUID(t *testing.T, obj map[string]jsontext.Value, key string) uuid.UUID {
	t.Helper()
	raw := requireJSONKey(t, obj, key)
	require.Equal(t, jsontext.Kind('"'), raw.Kind(), "key %q must be JSON string UUID, got %s", key, raw)
	var id uuid.UUID
	require.NoError(t, json.Unmarshal(raw, &id), "key %q must be valid UUID, got %s", key, raw)
	assert.NotEqual(t, uuid.Nil(), id, "key %q must be non-nil UUID", key)
	return id
}

func TestHumaExportReviewsSchemaVersionNegotiation(t *testing.T) {
	srv, db, _ := newTestServer(t)
	repo := testutil.CreateTestRepo(t, db)
	testutil.CreateCompletedReview(t, db, repo.ID, "export-schema-v2", "test-agent", "No issues found.")

	for _, path := range []string{
		"/api/export/reviews",
		"/api/export/reviews?schema_version=2",
	} {
		t.Run(path, func(t *testing.T) {
			rr := serveHuma(t, srv, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			var raw map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
			assert.Equal(t, int64(2), assertJSONInt64(t, raw, "schema_version"))
		})
	}

	for _, tc := range []struct {
		path    string
		wantMsg string
	}{
		{path: "/api/export/reviews?schema_version=0", wantMsg: "unsupported export schema_version"},
		{path: "/api/export/reviews?schema_version=1", wantMsg: "superset of version 1"},
		{path: "/api/export/reviews?schema_version=3", wantMsg: "unsupported export schema_version"},
		{path: "/api/export/reviews?schema_version=99", wantMsg: "unsupported export schema_version"},
		{path: "/api/export/reviews?schema_version=-1", wantMsg: "unsupported export schema_version"},
		{path: "/api/export/reviews?schema_version=abc", wantMsg: "invalid export schema_version"},
		{path: "/api/export/reviews?schema_version=2.0", wantMsg: "invalid export schema_version"},
		{path: "/api/export/reviews?schema_version=%202", wantMsg: "invalid export schema_version"},
		{path: "/api/export/reviews?schema_version=99999999999999999999999", wantMsg: "invalid export schema_version"},
		{path: "/api/export/reviews?schema_version=", wantMsg: "invalid export schema_version"},
		{path: "/api/export/reviews?schema_version=2&schema_version=3", wantMsg: "duplicate export schema_version"},
	} {
		t.Run("reject "+tc.path, func(t *testing.T) {
			rr := serveHuma(t, srv, http.MethodGet, tc.path, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), tc.wantMsg)
		})
	}
}

func TestHumaExportReviewsV2SchemaContract(t *testing.T) {
	srv, db, _ := newTestServer(t)

	repo := testutil.CreateTestRepo(t, db)
	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))
	localMachineID, err := db.GetMachineID()
	require.NoError(t, err)

	// Review 1: dirty review with unpopulated optional/nullable fields
	// (no commit_sha, no model, no branch, no PR, no token usage, no resume source)
	// and closed=true.
	dirtyJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:      repo.ID,
		GitRef:      "dirty",
		DiffContent: "diff --git a/README.md b/README.md\n",
		Agent:       "test-agent",
	})
	require.NoError(t, err)
	dirtyDoc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion,
		Summary:       "No issues found.",
		Verdict:       structuredreview.VerdictPass,
		Findings:      []structuredreview.Finding{},
	}

	// Review 2: CI panel run seeded strictly through storage.DB APIs.
	// - synthesis populates pr_number, pr_url, commit_sha, model, branch,
	//   resume_source_job_uuid, experiments, and numeric token/USD costs.
	// - reviewer-a populates model, resume_source_job_uuid, and numeric costs.
	// - reviewer-b leaves model, costs, and resume_source_job_uuid unpopulated,
	//   marks non_voting=true, and records both a fully populated finding and
	//   a finding with explicit null optional fields.
	const (
		panelBaseSHA   = "1111111111111111111111111111111111111111"
		panelCommitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		panelRangeRef  = panelBaseSHA + ".." + panelCommitSHA
	)
	commit, err := db.GetOrCreateCommit(repo.ID, panelCommitSHA, "Author", "panel commit", time.Now().UTC())
	require.NoError(t, err)
	resumeSourceID := uuid.New()
	expAssignment := &storage.ExperimentAssignmentInput{
		ExperimentID:        "exp-contract-v2",
		DefinitionHash:      "def-hash-1",
		DefinitionJSON:      `{"id":"exp-contract-v2"}`,
		Arm:                 "experiment",
		SubjectHash:         "subj-hash-1",
		EffectiveConfigHash: "cfg-hash-1",
		EffectiveConfigJSON: `{"arm":"experiment"}`,
	}
	created, memberJobs, synthJob, err := db.CreateCIPanelRun(
		"acme/widgets", 42, panelCommitSHA,
		[]storage.EnqueueOpts{
			{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: panelRangeRef,
				Branch: "main", Agent: "test-agent", Model: "gpt-5.5",
				ReviewType: "default", SessionID: "contract-session-member-a",
				ResumeSourceJobUUID: &resumeSourceID,
				PanelName:           "default", PanelMemberName: "reviewer-a", PanelMemberIndex: 0,
			},
			{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: panelRangeRef,
				Agent: "test-agent", ReviewType: "security",
				PanelName: "default", PanelMemberName: "reviewer-b", PanelMemberIndex: 1,
				NonVoting: true,
			},
		},
		storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: panelRangeRef,
			Branch: "main", Agent: "test-agent", Model: "gpt-5.5",
			SessionID:           "contract-session-synth",
			ResumeSourceJobUUID: &resumeSourceID,
			PanelName:           "default",
			Experiment:          expAssignment,
		},
	)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, memberJobs, 2)

	memberADoc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion,
		Summary:       "Looks clean.",
		Verdict:       structuredreview.VerdictPass,
		Findings:      []structuredreview.Finding{},
	}
	memberBDoc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion,
		Summary:       "Check input validation.",
		Verdict:       structuredreview.VerdictFail,
		Findings: []structuredreview.Finding{
			{
				Severity:   "high",
				Kind:       "defect",
				Topic:      "security",
				Confidence: 0.85,
				Problem:    "Missing validation.",
				Fix:        "Validate input.",
				File:       "auth/login.go",
				StartLine:  10,
				EndLine:    12,
				Location:   "auth/login.go:10-12",
			},
			{
				Severity: "low",
				Problem:  "Clarify comment.",
				Fix:      "Update comment wording.",
			},
		},
	}
	synthDoc := structuredreview.Document{
		SchemaVersion: structuredreview.SchemaVersion,
		Summary:       "One advisory note.",
		Verdict:       structuredreview.VerdictPass,
		Findings:      []structuredreview.Finding{},
		SourceLabels:  []string{"reviewer-a", "reviewer-b"},
	}

	completeNextWithDoc := func(expectedID int64, doc structuredreview.Document, verdict storage.Verdict, sessionID, tokenUsageJSON string) {
		t.Helper()
		raw, marshalErr := json.Marshal(doc)
		require.NoError(t, marshalErr)
		claimed, claimErr := db.ClaimJobContext(t.Context(), "worker-contract")
		require.NoError(t, claimErr)
		require.NotNil(t, claimed)
		require.Equal(t, expectedID, claimed.ID)
		require.NoError(t, db.CompleteJobResult(claimed.ID, "test-agent", storage.ReviewCompletion{
			Output:           doc.Markdown(""),
			Verdict:          verdict,
			StructuredOutput: jsontext.Value(raw),
		}))
		if tokenUsageJSON != "" {
			require.NoError(t, db.SaveJobTokenUsage(claimed.ID, sessionID, tokenUsageJSON))
		}
	}
	completeNextWithDoc(dirtyJob.ID, dirtyDoc, storage.VerdictPass, "", "")
	require.NoError(t, db.MarkReviewClosedByJobID(dirtyJob.ID, true))
	completeNextWithDoc(
		memberJobs[0].ID, memberADoc, storage.VerdictPass,
		"contract-session-member-a",
		`{"input_tokens":400,"total_output_tokens":80,"cost_usd":0.12,"has_cost":true}`,
	)
	completeNextWithDoc(memberJobs[1].ID, memberBDoc, storage.VerdictFail, "", "")
	require.NotNil(t, synthJob.PanelRunUUID)
	require.NoError(t, db.MaybeReleasePanelSynthesisContext(t.Context(), *synthJob.PanelRunUUID))
	completeNextWithDoc(
		synthJob.ID, synthDoc, storage.VerdictPass,
		"contract-session-synth",
		`{"input_tokens":1200,"total_output_tokens":340,"cost_usd":0.42,"has_cost":true}`,
	)

	// Review 3: pulled historical legacy review (schema_version 0, unknown
	// verdict, distinct source_machine_id, null duration_ms).
	remoteMachineID := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	legacyJobUUID := uuid.New()
	legacyReviewID := uuid.New()
	require.NoError(t, db.UpsertPulledJob(storage.PulledJob{
		UUID:            legacyJobUUID,
		RepoIdentity:    "github.com/acme/widgets",
		GitRef:          "HEAD",
		Agent:           "test-agent",
		JobType:         storage.JobTypeReview,
		Status:          string(storage.JobStatusDone),
		SourceMachineID: remoteMachineID,
		EnqueuedAt:      now.Add(-2 * time.Minute),
		UpdatedAt:       now.Add(-2 * time.Minute),
	}, repo.ID, nil))
	require.NoError(t, db.UpsertPulledReview(storage.PulledReview{
		UUID:               legacyReviewID,
		JobUUID:            legacyJobUUID,
		JobEnqueuedAt:      now.Add(-2 * time.Minute),
		JobSourceMachineID: remoteMachineID,
		Agent:              "test-agent",
		StructuredOutput:   jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"Historical review text.","recorded_verdict":null}}`),
		VerdictBool:        nil,
		UpdatedByMachineID: localMachineID,
		CreatedAt:          now.Add(-2 * time.Minute),
		UpdatedAt:          now.Add(-2 * time.Minute),
	}))

	// Review 4: completed review without a stored structured document (e.g. a
	// row predating structured_output; content and document must both be
	// explicit JSON null even in the content profile).
	noDocJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:      repo.ID,
		GitRef:      "dirty",
		DiffContent: "diff --git a/nodoc.txt b/nodoc.txt\n",
		Agent:       "test-agent",
	})
	require.NoError(t, err)
	completeNextWithDoc(noDocJob.ID, dirtyDoc, storage.VerdictPass, "", "")
	noDocStoredReview, err := db.GetReviewByJobID(noDocJob.ID)
	require.NoError(t, err)
	require.NotNil(t, noDocStoredReview.UUID)
	noDocReviewID := *noDocStoredReview.UUID
	_, err = db.ExecContext(t.Context(), `UPDATE reviews SET structured_output = NULL WHERE job_id = ?`, noDocJob.ID)
	require.NoError(t, err)

	dirtyStoredReview, err := db.GetReviewByJobID(dirtyJob.ID)
	require.NoError(t, err)
	require.NotNil(t, dirtyStoredReview.UUID)
	synthStoredReview, err := db.GetReviewByJobID(synthJob.ID)
	require.NoError(t, err)
	require.NotNil(t, synthStoredReview.UUID)

	for _, profile := range []string{"content", "metadata"} {
		t.Run(profile, func(t *testing.T) {
			assert := assert.New(t)
			rr := serveHuma(t, srv, http.MethodGet, "/api/export/reviews?schema_version=2&profile="+profile, nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			// Inspect raw JSON maps rather than production structs so wire type
			// changes (e.g., stringified costs or omitted nulls) are caught while
			// unknown additive keys remain allowed.
			var header map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &header))
			for _, key := range exportReviewsV2HeaderKeys {
				requireJSONKey(t, header, key)
			}
			assert.Equal(int64(2), assertJSONInt64(t, header, "schema_version"))
			assert.Equal("roborev", assertJSONString(t, header, "tool"))
			assert.NotEmpty(assertJSONString(t, header, "tool_version"))
			assertRFC3339String(t, header, "generated_at")
			assertJSONUUID(t, header, "database_id")
			assert.Equal(profile, assertJSONString(t, header, "profile"))
			assert.False(assertJSONBool(t, header, "truncated"))
			assert.NotEmpty(assertJSONString(t, header, "next_cursor"))

			window := assertJSONObject(t, header, "window")
			for _, key := range exportReviewsV2WindowKeys {
				requireJSONKey(t, window, key)
			}
			assert.Equal("completed_at", assertJSONString(t, window, "field"))
			assertJSONNull(t, window, "since")
			assertJSONNull(t, window, "until")

			reviews := assertJSONArray(t, header, "reviews")
			require.Len(t, reviews, 4, "panel members must export under subagents, not as top-level reviews")

			byReviewID := make(map[uuid.UUID]map[string]jsontext.Value, len(reviews))
			for _, review := range reviews {
				for _, key := range exportReviewsV2ReviewKeys {
					requireJSONKey(t, review, key)
				}
				reviewID := assertJSONUUID(t, review, "review_id")
				assertJSONUUID(t, review, "source_machine_id")
				assert.Equal(localMachineID, assertJSONUUID(t, review, "exported_by_machine_id"))
				assert.Equal("done", assertJSONString(t, review, "status"))
				assert.Contains([]string{"pass", "fail", "unknown"}, assertJSONString(t, review, "verdict"))
				assertRFC3339String(t, review, "created_at")
				assertRFC3339String(t, review, "completed_at")
				assertRFC3339String(t, review, "updated_at")
				assert.NotEmpty(assertJSONString(t, review, "project"))
				assert.Equal("github.com/acme/widgets", assertJSONString(t, review, "repo"))
				assert.Equal("test-agent", assertJSONString(t, review, "agent"))
				assertJSONArray(t, review, "experiments")
				byReviewID[reviewID] = review

				if profile == "metadata" {
					assertJSONNull(t, review, "document")
					assertJSONNull(t, review, "content")
				}
			}

			// Review 1 (dirty): absent nullable fields must be explicit JSON
			// null, never zero values, empty strings, or omitted keys.
			plain := byReviewID[*dirtyStoredReview.UUID]
			require.NotNil(t, plain)
			assert.Equal(localMachineID, assertJSONUUID(t, plain, "source_machine_id"))
			assert.GreaterOrEqual(assertJSONInt64(t, plain, "duration_ms"), int64(0))
			assert.True(assertJSONBool(t, plain, "closed"))
			assertJSONNull(t, plain, "commit_sha")
			assertJSONNull(t, plain, "branch")
			assertJSONNull(t, plain, "model")
			assertJSONNull(t, plain, "pr_number")
			assertJSONNull(t, plain, "pr_url")
			assertJSONNull(t, plain, "resume_source_job_uuid")
			assert.Empty(assertJSONArray(t, plain, "subagents"))
			assert.Empty(assertJSONArray(t, plain, "experiments"))
			plainCost := assertJSONObject(t, plain, "cost")
			for _, key := range exportReviewsV2CostKeys {
				assertJSONNull(t, plainCost, key)
			}
			if profile == "content" {
				assert.NotEmpty(assertJSONString(t, plain, "content"))
				docObj := assertJSONObject(t, plain, "document")
				for _, key := range exportReviewsV2DocumentKeys {
					requireJSONKey(t, docObj, key)
				}
				// Export v2 freezes structured documents at literal version 3.
				assert.Equal(int64(3), assertJSONInt64(t, docObj, "schema_version"))
				assert.Equal("No issues found.", assertJSONString(t, docObj, "summary"))
				assert.Empty(assertJSONArray(t, docObj, "findings"))
			}

			// Review 2 (CI panel synthesis): populated optional fields must use
			// exact JSON wire types (numbers for costs/duration/pr_number,
			// strings for commit_sha/pr_url/model/branch/UUID/experiments,
			// booleans for closed/non_voting).
			panel := byReviewID[*synthStoredReview.UUID]
			require.NotNil(t, panel)
			assert.GreaterOrEqual(assertJSONInt64(t, panel, "duration_ms"), int64(0))
			assert.False(assertJSONBool(t, panel, "closed"))
			assert.Equal(panelCommitSHA, assertJSONString(t, panel, "commit_sha"))
			assert.Equal("main", assertJSONString(t, panel, "branch"))
			assert.Equal("gpt-5.5", assertJSONString(t, panel, "model"))
			assert.Equal(int64(42), assertJSONInt64(t, panel, "pr_number"))
			assert.Equal("https://github.com/acme/widgets/pull/42", assertJSONString(t, panel, "pr_url"))
			assert.Equal(resumeSourceID.String(), assertJSONString(t, panel, "resume_source_job_uuid"))

			panelCost := assertJSONObject(t, panel, "cost")
			for _, key := range exportReviewsV2CostKeys {
				requireJSONKey(t, panelCost, key)
			}
			assert.Equal(int64(1200), assertJSONInt64(t, panelCost, "tokens_in"))
			assert.Equal(int64(340), assertJSONInt64(t, panelCost, "tokens_out"))
			assert.InDelta(0.42, assertJSONFloat64(t, panelCost, "usd"), 1e-9)

			experiments := assertJSONArray(t, panel, "experiments")
			require.Len(t, experiments, 1)
			for _, key := range exportReviewsV2ExperimentKeys {
				assert.NotEmpty(assertJSONString(t, experiments[0], key))
			}
			assert.Equal("exp-contract-v2", assertJSONString(t, experiments[0], "id"))
			assert.Equal("experiment", assertJSONString(t, experiments[0], "arm"))

			if profile == "content" {
				assert.NotEmpty(assertJSONString(t, panel, "content"))
				docObj := assertJSONObject(t, panel, "document")
				for _, key := range exportReviewsV2DocumentKeys {
					requireJSONKey(t, docObj, key)
				}
				assert.Equal(int64(3), assertJSONInt64(t, docObj, "schema_version"))
				assert.Equal("One advisory note.", assertJSONString(t, docObj, "summary"))
				assert.Empty(assertJSONArray(t, docObj, "findings"))
			}

			subagents := assertJSONArray(t, panel, "subagents")
			require.Len(t, subagents, 2)
			byMember := make(map[string]map[string]jsontext.Value, len(subagents))
			for _, sub := range subagents {
				for _, key := range exportReviewsV2SubagentKeys {
					requireJSONKey(t, sub, key)
				}
				assertJSONUUID(t, sub, "review_id")
				assert.Equal("test-agent", assertJSONString(t, sub, "agent"))
				assertRFC3339String(t, sub, "completed_at")
				assert.GreaterOrEqual(assertJSONInt64(t, sub, "duration_ms"), int64(0))

				if profile == "metadata" {
					assertJSONNull(t, sub, "document")
					assertJSONNull(t, sub, "content")
				} else {
					assert.NotEmpty(assertJSONString(t, sub, "content"))
					subDocObj := assertJSONObject(t, sub, "document")
					for _, key := range exportReviewsV2DocumentKeys {
						requireJSONKey(t, subDocObj, key)
					}
					assert.Equal(int64(3), assertJSONInt64(t, subDocObj, "schema_version"))
				}
				byMember[assertJSONString(t, sub, "name")] = sub
			}

			// reviewer-a: populated model, numeric costs, resume source, non_voting=false.
			memberA := byMember["reviewer-a"]
			require.NotNil(t, memberA)
			assert.False(assertJSONBool(t, memberA, "non_voting"))
			assert.Equal("pass", assertJSONString(t, memberA, "verdict"))
			assert.Equal("default", assertJSONString(t, memberA, "review_type"))
			assert.Equal("gpt-5.5", assertJSONString(t, memberA, "model"))
			assert.Equal(resumeSourceID.String(), assertJSONString(t, memberA, "resume_source_job_uuid"))
			memberACost := assertJSONObject(t, memberA, "cost")
			assert.Equal(int64(400), assertJSONInt64(t, memberACost, "tokens_in"))
			assert.Equal(int64(80), assertJSONInt64(t, memberACost, "tokens_out"))
			assert.InDelta(0.12, assertJSONFloat64(t, memberACost, "usd"), 1e-9)

			// reviewer-b: unpopulated optional fields must be explicit JSON null,
			// non_voting=true, and its v3 findings cover both populated and null fields.
			memberB := byMember["reviewer-b"]
			require.NotNil(t, memberB)
			assert.True(assertJSONBool(t, memberB, "non_voting"))
			assert.Equal("fail", assertJSONString(t, memberB, "verdict"))
			assert.Equal("security", assertJSONString(t, memberB, "review_type"))
			assertJSONNull(t, memberB, "model")
			assertJSONNull(t, memberB, "resume_source_job_uuid")
			memberBCost := assertJSONObject(t, memberB, "cost")
			for _, key := range exportReviewsV2CostKeys {
				assertJSONNull(t, memberBCost, key)
			}
			if profile == "content" {
				memberBDocObj := assertJSONObject(t, memberB, "document")
				findings := assertJSONArray(t, memberBDocObj, "findings")
				require.Len(t, findings, 2)

				populated := findings[0]
				for _, key := range exportReviewsV2FindingKeys {
					requireJSONKey(t, populated, key)
				}
				assert.Equal("high", assertJSONString(t, populated, "severity"))
				assert.Equal("defect", assertJSONString(t, populated, "kind"))
				assert.Equal("security", assertJSONString(t, populated, "topic"))
				assert.InDelta(0.85, assertJSONFloat64(t, populated, "confidence"), 1e-9)
				assert.Equal("Missing validation.", assertJSONString(t, populated, "problem"))
				assert.Equal("Validate input.", assertJSONString(t, populated, "fix"))
				assert.Equal("auth/login.go", assertJSONString(t, populated, "file"))
				assert.Equal(int64(10), assertJSONInt64(t, populated, "start_line"))
				assert.Equal(int64(12), assertJSONInt64(t, populated, "end_line"))
				assert.Equal("auth/login.go:10-12", assertJSONString(t, populated, "location"))

				nullable := findings[1]
				for _, key := range exportReviewsV2FindingKeys {
					requireJSONKey(t, nullable, key)
				}
				assert.Equal("low", assertJSONString(t, nullable, "severity"))
				assert.Equal("Clarify comment.", assertJSONString(t, nullable, "problem"))
				assert.Equal("Update comment wording.", assertJSONString(t, nullable, "fix"))
				assertJSONNull(t, nullable, "kind")
				assertJSONNull(t, nullable, "topic")
				assertJSONNull(t, nullable, "confidence")
				assertJSONNull(t, nullable, "file")
				assertJSONNull(t, nullable, "start_line")
				assertJSONNull(t, nullable, "end_line")
				assertJSONNull(t, nullable, "location")
			}

			// Review 3 (pulled legacy): distinct source_machine_id, verdict
			// "unknown", null duration_ms, and legacy document version 0.
			legacy := byReviewID[legacyReviewID]
			require.NotNil(t, legacy)
			assert.Equal(remoteMachineID, assertJSONUUID(t, legacy, "source_machine_id"))
			assert.Equal("unknown", assertJSONString(t, legacy, "verdict"))
			assertJSONNull(t, legacy, "duration_ms")
			if profile == "content" {
				assert.Contains(assertJSONString(t, legacy, "content"), "Historical review text.")
				legacyDocObj := assertJSONObject(t, legacy, "document")
				for _, key := range exportReviewsV2LegacyDocumentKeys {
					requireJSONKey(t, legacyDocObj, key)
				}
				assert.Equal(int64(0), assertJSONInt64(t, legacyDocObj, "schema_version"))
				legacyPayload := assertJSONObject(t, legacyDocObj, "legacy")
				for _, key := range exportReviewsV2LegacyPayloadKeys {
					requireJSONKey(t, legacyPayload, key)
				}
				assert.Equal("Historical review text.", assertJSONString(t, legacyPayload, "markdown"))
				assertJSONNull(t, legacyPayload, "recorded_verdict")
			}

			// Review 4 (no stored document): content and document must both be
			// explicit JSON null even in the content profile.
			noDoc := byReviewID[noDocReviewID]
			require.NotNil(t, noDoc)
			assert.Equal("pass", assertJSONString(t, noDoc, "verdict"))
			assertJSONNull(t, noDoc, "content")
			assertJSONNull(t, noDoc, "document")
		})
	}

	t.Run("window bounds pagination and empty page", func(t *testing.T) {
		assert := assert.New(t)

		// Bounded window with limit=1 must populate window.since/until as
		// strings, set truncated=true, and return a usable next_cursor.
		rr := serveHuma(t, srv, http.MethodGet,
			"/api/export/reviews?schema_version=2&profile=metadata&since=2020-01-01&until=2099-01-01&limit=1", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var page1 map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page1))
		assert.Equal(int64(2), assertJSONInt64(t, page1, "schema_version"))
		assert.True(assertJSONBool(t, page1, "truncated"))
		cursor1 := assertJSONString(t, page1, "next_cursor")
		require.NotEmpty(t, cursor1)

		window1 := assertJSONObject(t, page1, "window")
		assertRFC3339String(t, window1, "since")
		assertRFC3339String(t, window1, "until")

		reviews1 := assertJSONArray(t, page1, "reviews")
		require.Len(t, reviews1, 1)
		firstID := assertJSONUUID(t, reviews1[0], "review_id")

		// Resume with cursor + schema_version=2 advances to a distinct review.
		rr2 := serveHuma(t, srv, http.MethodGet,
			"/api/export/reviews?schema_version=2&profile=metadata&limit=1&cursor="+url.QueryEscape(cursor1), nil)
		require.Equal(t, http.StatusOK, rr2.Code, rr2.Body.String())

		var page2 map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &page2))
		assert.Equal(int64(2), assertJSONInt64(t, page2, "schema_version"))
		reviews2 := assertJSONArray(t, page2, "reviews")
		require.Len(t, reviews2, 1)
		secondID := assertJSONUUID(t, reviews2[0], "review_id")
		assert.NotEqual(firstID, secondID)

		// Empty export must return reviews: [], truncated: false, next_cursor: null.
		rrEmpty := serveHuma(t, srv, http.MethodGet,
			"/api/export/reviews?schema_version=2&since=2099-01-01", nil)
		require.Equal(t, http.StatusOK, rrEmpty.Code, rrEmpty.Body.String())

		var emptyPage map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(rrEmpty.Body.Bytes(), &emptyPage))
		assert.Equal(int64(2), assertJSONInt64(t, emptyPage, "schema_version"))
		assert.False(assertJSONBool(t, emptyPage, "truncated"))
		assertJSONNull(t, emptyPage, "next_cursor")
		assert.Empty(assertJSONArray(t, emptyPage, "reviews"))
	})
}
