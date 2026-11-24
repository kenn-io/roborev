package daemon

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
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
			assert.Equal(t, int64(ExportReviewsSchemaVersion), assertJSONInt64(t, raw, "schema_version"))
		})
	}

	for _, path := range []string{
		"/api/export/reviews?schema_version=0",
		"/api/export/reviews?schema_version=1",
		"/api/export/reviews?schema_version=3",
		"/api/export/reviews?schema_version=99",
		"/api/export/reviews?schema_version=-1",
	} {
		t.Run("reject "+path, func(t *testing.T) {
			rr := serveHuma(t, srv, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "unsupported export schema_version")
		})
	}
}

func TestHumaExportReviewsV2SchemaContract(t *testing.T) {
	srv, db, _ := newTestServer(t)

	repo := testutil.CreateTestRepo(t, db)
	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))

	// Review 1: dirty review with unpopulated optional/nullable fields
	// (no commit_sha, no model, no branch, no token usage, no resume source)
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

	// Review 2: panel run seeded strictly through storage.DB APIs.
	// - synthesis + reviewer-a populate commit_sha (via range GitRef), model,
	//   branch, resume_source_job_uuid, and numeric token/USD costs.
	// - reviewer-b leaves model, costs, and resume_source_job_uuid unpopulated
	//   and marks non_voting=true.
	const (
		panelBaseSHA   = "1111111111111111111111111111111111111111"
		panelCommitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		panelRangeRef  = panelBaseSHA + ".." + panelCommitSHA
	)
	commit, err := db.GetOrCreateCommit(repo.ID, panelCommitSHA, "Author", "panel commit", time.Now().UTC())
	require.NoError(t, err)
	panelRunID := uuid.New()
	resumeSourceID := uuid.New()
	memberJobs, synthJob, err := db.EnqueuePanelRun(
		[]storage.EnqueueOpts{
			{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: panelRangeRef,
				Branch: "main", Agent: "test-agent", Model: "gpt-5.5",
				ReviewType: "default", SessionID: "contract-session-member-a",
				ResumeSourceJobUUID: &resumeSourceID,
				PanelRunUUID:        &panelRunID, PanelName: "default",
				PanelMemberName: "reviewer-a", PanelMemberIndex: 0,
			},
			{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: panelRangeRef,
				Agent: "test-agent", ReviewType: "security",
				PanelRunUUID: &panelRunID, PanelName: "default",
				PanelMemberName: "reviewer-b", PanelMemberIndex: 1,
				NonVoting: true,
			},
		},
		storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: panelRangeRef,
			Branch: "main", Agent: "test-agent", Model: "gpt-5.5",
			SessionID:           "contract-session-synth",
			ResumeSourceJobUUID: &resumeSourceID,
			PanelRunUUID:        &panelRunID, PanelName: "default",
		},
	)
	require.NoError(t, err)
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
		Findings: []structuredreview.Finding{{
			Severity: "medium",
			Problem:  "Missing validation.",
			Fix:      "Validate input.",
		}},
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
			require.Len(t, reviews, 2, "panel members must export under subagents, not as top-level reviews")

			byReviewID := make(map[uuid.UUID]map[string]jsontext.Value, len(reviews))
			for _, review := range reviews {
				for _, key := range exportReviewsV2ReviewKeys {
					requireJSONKey(t, review, key)
				}
				reviewID := assertJSONUUID(t, review, "review_id")
				assert.Equal("done", assertJSONString(t, review, "status"))
				assert.Contains([]string{"pass", "fail", "unknown"}, assertJSONString(t, review, "verdict"))
				assertRFC3339String(t, review, "created_at")
				assertRFC3339String(t, review, "completed_at")
				assertRFC3339String(t, review, "updated_at")
				assert.NotEmpty(assertJSONString(t, review, "project"))
				assert.Equal("github.com/acme/widgets", assertJSONString(t, review, "repo"))
				assert.Equal("test-agent", assertJSONString(t, review, "agent"))
				assert.GreaterOrEqual(assertJSONInt64(t, review, "duration_ms"), int64(0))
				assertJSONNull(t, review, "pr_number")
				assertJSONNull(t, review, "pr_url")
				assert.Empty(assertJSONArray(t, review, "experiments"))

				byReviewID[reviewID] = review

				if profile == "metadata" {
					assertJSONNull(t, review, "document")
					assertJSONNull(t, review, "content")
				} else {
					assert.NotEmpty(assertJSONString(t, review, "content"))
					docObj := assertJSONObject(t, review, "document")
					assert.Equal(int64(structuredreview.SchemaVersion), assertJSONInt64(t, docObj, "schema_version"))
					assert.NotEmpty(assertJSONString(t, docObj, "summary"))
					assertJSONArray(t, docObj, "findings")
					_, decodeErr := structuredreview.Decode(review["document"])
					require.NoError(t, decodeErr)
				}
			}

			// Dirty review: absent nullable fields must be explicit JSON null,
			// never zero values, empty strings, or omitted keys.
			plain := byReviewID[*dirtyStoredReview.UUID]
			require.NotNil(t, plain)
			assert.True(assertJSONBool(t, plain, "closed"))
			assertJSONNull(t, plain, "commit_sha")
			assertJSONNull(t, plain, "branch")
			assertJSONNull(t, plain, "model")
			assertJSONNull(t, plain, "resume_source_job_uuid")
			assert.Empty(assertJSONArray(t, plain, "subagents"))

			plainCost := assertJSONObject(t, plain, "cost")
			for _, key := range exportReviewsV2CostKeys {
				assertJSONNull(t, plainCost, key)
			}

			// Panel synthesis review: populated optional fields must use their
			// exact JSON wire types (numbers for costs/duration, strings for
			// commit_sha/model/branch/UUID, booleans for closed/non_voting).
			panel := byReviewID[*synthStoredReview.UUID]
			require.NotNil(t, panel)
			assert.False(assertJSONBool(t, panel, "closed"))
			assert.Equal(panelCommitSHA, assertJSONString(t, panel, "commit_sha"))
			assert.Equal("main", assertJSONString(t, panel, "branch"))
			assert.Equal("gpt-5.5", assertJSONString(t, panel, "model"))
			assert.Equal(resumeSourceID.String(), assertJSONString(t, panel, "resume_source_job_uuid"))

			panelCost := assertJSONObject(t, panel, "cost")
			for _, key := range exportReviewsV2CostKeys {
				requireJSONKey(t, panelCost, key)
			}
			assert.Equal(int64(1200), assertJSONInt64(t, panelCost, "tokens_in"))
			assert.Equal(int64(340), assertJSONInt64(t, panelCost, "tokens_out"))
			assert.InDelta(0.42, assertJSONFloat64(t, panelCost, "usd"), 1e-9)

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
					assert.Equal(int64(structuredreview.SchemaVersion), assertJSONInt64(t, subDocObj, "schema_version"))
					_, decodeErr := structuredreview.Decode(sub["document"])
					require.NoError(t, decodeErr)
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
			// and non_voting=true.
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
		})
	}
}
