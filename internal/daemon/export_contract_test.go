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

			var body struct {
				SchemaVersion int `json:"schema_version"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
			assert.Equal(t, ExportReviewsSchemaVersion, body.SchemaVersion)
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
	assert := assert.New(t)

	repo := testutil.CreateTestRepo(t, db)
	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))

	// Plain completed review with a closed state transition.
	plainJob := testutil.CreateCompletedReview(t, db, repo.ID, "contract-plain", "test-agent", "No issues found.")
	require.NoError(t, db.MarkReviewClosedByJobID(plainJob.ID, true))

	// Panel run (voting member + non-voting member + synthesis) seeded strictly
	// through storage.DB APIs so internal table migrations never break this test.
	commit, err := db.GetOrCreateCommit(repo.ID, "contract-panel", "Author", "panel commit", time.Now().UTC())
	require.NoError(t, err)
	panelRunID := uuid.New()
	memberJobs, synthJob, err := db.EnqueuePanelRun(
		[]storage.EnqueueOpts{
			{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA,
				Agent: "test-agent", ReviewType: "default",
				PanelRunUUID: &panelRunID, PanelName: "default", PanelMemberName: "reviewer-a", PanelMemberIndex: 0,
			},
			{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA,
				Agent: "test-agent", ReviewType: "security",
				PanelRunUUID: &panelRunID, PanelName: "default", PanelMemberName: "reviewer-b", PanelMemberIndex: 1,
				NonVoting: true,
			},
		},
		storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA,
			Agent: "test-agent", PanelRunUUID: &panelRunID, PanelName: "default",
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

	completeNextWithDoc := func(expectedID int64, doc structuredreview.Document, verdict storage.Verdict) {
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
	}
	completeNextWithDoc(memberJobs[0].ID, memberADoc, storage.VerdictPass)
	completeNextWithDoc(memberJobs[1].ID, memberBDoc, storage.VerdictFail)
	require.NotNil(t, synthJob.PanelRunUUID)
	require.NoError(t, db.MaybeReleasePanelSynthesisContext(t.Context(), *synthJob.PanelRunUUID))
	completeNextWithDoc(synthJob.ID, synthDoc, storage.VerdictPass)

	for _, profile := range []string{"content", "metadata"} {
		t.Run(profile, func(t *testing.T) {
			rr := serveHuma(t, srv, http.MethodGet, "/api/export/reviews?schema_version=2&profile="+profile, nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			var header map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &header))
			for _, key := range exportReviewsV2HeaderKeys {
				assert.Contains(header, key, "v2 header must keep key %q", key)
			}
			assert.Equal("2", string(header["schema_version"]))

			var window map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(header["window"], &window))
			for _, key := range exportReviewsV2WindowKeys {
				assert.Contains(window, key, "v2 window must keep key %q", key)
			}

			var reviews []map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(header["reviews"], &reviews))
			require.Len(t, reviews, 2, "panel members must export under subagents, not as top-level reviews")

			sawPanel := false
			for _, review := range reviews {
				for _, key := range exportReviewsV2ReviewKeys {
					assert.Contains(review, key, "v2 review must keep key %q", key)
				}

				var reviewID uuid.UUID
				require.NoError(t, json.Unmarshal(review["review_id"], &reviewID))
				assert.NotEqual(uuid.Nil(), reviewID)

				var closed bool
				require.NoError(t, json.Unmarshal(review["closed"], &closed))

				var updatedAt string
				require.NoError(t, json.Unmarshal(review["updated_at"], &updatedAt))
				_, parseErr := time.Parse(time.RFC3339, updatedAt)
				require.NoError(t, parseErr, "updated_at must be RFC3339")

				var cost map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(review["cost"], &cost))
				for _, key := range exportReviewsV2CostKeys {
					assert.Contains(cost, key, "v2 cost must keep key %q", key)
				}

				// subagents must always be a JSON array, never null.
				assert.NotEqual("null", string(review["subagents"]))
				var subagents []map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(review["subagents"], &subagents))

				if profile == "metadata" {
					assert.Equal("null", string(review["document"]))
					assert.Equal("null", string(review["content"]))
				} else {
					assert.NotEqual("null", string(review["document"]))
					_, decodeErr := structuredreview.Decode(review["document"])
					require.NoError(t, decodeErr)
				}

				if len(subagents) == 0 {
					continue
				}
				sawPanel = true
				require.Len(t, subagents, 2)
				for _, sub := range subagents {
					for _, key := range exportReviewsV2SubagentKeys {
						assert.Contains(sub, key, "v2 subagent must keep key %q", key)
					}
					var subCost map[string]jsontext.Value
					require.NoError(t, json.Unmarshal(sub["cost"], &subCost))
					for _, key := range exportReviewsV2CostKeys {
						assert.Contains(subCost, key, "v2 subagent cost must keep key %q", key)
					}
					if profile == "metadata" {
						assert.Equal("null", string(sub["document"]))
						assert.Equal("null", string(sub["content"]))
					} else {
						assert.NotEqual("null", string(sub["document"]))
						_, decodeErr := structuredreview.Decode(sub["document"])
						require.NoError(t, decodeErr)
					}
				}
			}
			assert.True(sawPanel, "expected panel review with subagents")
		})
	}
}
