package daemon

import (
	"encoding/json/jsontext"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/provenance"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSessionContextJobEligibility(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.CommitFile("feature.txt", "feature\n", "Feature\n\nAgent-Session: pi:writer")
	for _, tc := range []struct {
		name, kind, source string
		want               bool
	}{
		{"local", storage.JobTypeReview, "", true},
		{"local range", storage.JobTypeRange, "", true},
		{"CI", storage.JobTypeReview, storage.JobSourceCI, false},
		{"dirty", storage.JobTypeDirty, "", false},
		{"goal", storage.JobTypeGoalReview, "", false},
		{"task", storage.JobTypeTask, "", false},
		{"fix", storage.JobTypeFix, "", false},
		{"classifier", storage.JobTypeClassify, "", false},
		{"synthesis", storage.JobTypeSynthesis, "", false},
		{"rebuttal", storage.JobTypeReview, storage.JobSourceRebuttal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; w.WriteHeader(404) }))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.SessionContext.Mode = "on"
			cfg.AgentsView.URL = srv.URL
			wp := new(WorkerPool)
			job := &storage.ReviewJob{JobType: tc.kind, Source: tc.source, GitRef: sha}
			checkout := preparedJobCheckout{promptRepoPath: repo.Path(), configRepoPath: repo.Path()}
			b := wp.basePromptBuilderForJob(t.Context(), checkout, job, cfg)
			output, err := b.Build(sha, 0, "test", "", "")
			require.NoError(t, err)
			if tc.want {
				assert.Contains(t, output, "Agent sessions behind these commits")
				assert.Equal(t, 1, requests)
			} else {
				assert.NotContains(t, output, "Agent sessions behind these commits")
				assert.Zero(t, requests)
			}
		})
	}
}

func TestDoctorSessionContextCapability(t *testing.T) {
	for _, status := range []int{200, 404, 401, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "POST", r.Method)
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.JSONEq(t, `{"shas":[]}`, string(body))
				w.WriteHeader(status)
				if status == 200 {
					_, _ = w.Write([]byte(`{"commits":{}}`))
				}
			}))
			defer srv.Close()
			cfg := config.DefaultConfig()
			cfg.SessionContext.Mode = "on"
			cfg.AgentsView.URL = srv.URL
			result := ResolveDoctorSessionContext(t.Context(), "", nil, cfg, new(provenance.Client))
			assert.Equal(t, "on", result.Mode)
			assert.Equal(t, 1, calls)
			switch status {
			case 200:
				assert.Equal(t, "supported", result.Status)
			case 404:
				assert.Equal(t, "unsupported", result.Status)
			default:
				assert.Equal(t, "unavailable", result.Status)
			}
			cfg.SessionContext.Mode = "off"
			result = ResolveDoctorSessionContext(t.Context(), "", nil, cfg, new(provenance.Client))
			assert.Equal(t, "off", result.Status)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestSessionProvenanceProjectionBoundaries(t *testing.T) {
	capture := []provenance.Commit{{CommitSHA: "0123456789abcdef0123456789abcdef01234567", Coverage: "http", Sessions: []provenance.Session{{SessionID: "pi:writer", Agent: "pi", Source: "trailer", Availability: "available"}}}}
	job := storage.ReviewJob{SessionProvenance: capture, Status: storage.JobStatusDone}
	assert.Equal(t, capture, projectReviewJob(job).SessionProvenance)
	assert.Equal(t, capture, projectBrowserReviewJob(job, BrowserPrincipal{Local: true}).SessionProvenance)
	jobs := []storage.ReviewJob{job}
	stripJobPrompts(jobs)
	assert.Empty(t, jobs[0].SessionProvenance)
	sanitizeRemoteJob(&job)
	assert.Empty(t, job.SessionProvenance)
}

func TestWorkerPersistsSessionProvenance(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	defer tc.DB.Close()
	sha := tc.GitRepo.CommitFile("intent.txt", "intent\n", "Preserve intent\n\nAgent-Session: pi:writer")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	cfg := config.DefaultConfig()
	cfg.SessionContext.Mode = "on"
	cfg.AgentsView.URL = srv.URL
	tc.Pool.cfgGetter = NewStaticConfig(cfg)
	const reviewer = "session-context-fixture"
	agent.RegisterForTest(t, &structuredWorkerTestAgent{name: reviewer, result: jsontext.Value(`{"schema_version":3,"summary":"No issues found.","verdict":"pass","findings":[]}`)})
	claimed := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, reviewer)
	tc.Pool.processJob(testWorkerID, claimed)
	stored, err := tc.DB.GetReviewByJobID(claimed.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.Job)
	require.Len(t, stored.Job.SessionProvenance, 1)
	assert.Equal(t, sha, stored.Job.SessionProvenance[0].CommitSHA)
	assert.Equal(t, "pi:writer", stored.Job.SessionProvenance[0].Sessions[0].SessionID)
	assert.Contains(t, stored.Prompt, "Agent sessions behind these commits")
}
