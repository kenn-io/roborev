package daemon

import (
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

const binaryEvidenceTypesConfig = `[review.types.exhibits]
template = "rubric.md"
binary_evidence = true

[review.types.plain]
template = "rubric.md"
`

func TestProcessJobBinaryEvidenceOnlyForEnablingType(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	// Git for Windows routes *.pdf through a text converter that rejects these
	// synthetic bytes; review the fixture as plain binary on every host.
	require.NoError(t, os.WriteFile(filepath.Join(tc.GitRepo.GitDir, "info", "attributes"), []byte("*.pdf -diff\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(binaryEvidenceTypesConfig), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "rubric.md"), []byte("Check the exhibits.\n"), 0o644))
	sha := tc.GitRepo.CommitFile("exhibit.pdf", "%PDF\x00committed", "add exhibit")
	// The reviewer must get the committed bytes, not this later edit.
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "exhibit.pdf"), []byte("%PDF\x00edited"), 0o644))
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)

	pathPattern := regexp.MustCompile(`"exhibit.pdf" \(added\): read ("[^"]+")`)
	for _, tt := range []struct {
		reviewType string
		evidence   bool
	}{{reviewType: "exhibits", evidence: true}, {reviewType: "plain"}} {
		t.Run(tt.reviewType, func(t *testing.T) {
			agentName := "evidence-" + tt.reviewType
			var saved string
			var savedData []byte
			agent.RegisterForTest(t, &structuredWorkerTestAgent{
				name:   agentName,
				result: jsontext.Value(`{"schema_version":2,"summary":"No findings.","verdict":"pass","findings":[]}`),
				onPrompt: func(_, prompt string) {
					if !tt.evidence {
						assert.NotContains(t, prompt, "Committed review evidence")
						return
					}
					match := pathPattern.FindStringSubmatch(prompt)
					require.NotNil(t, match, "prompt lists the binary file")
					var readErr error
					saved, readErr = strconv.Unquote(match[1])
					require.NoError(t, readErr)
					savedData, readErr = os.ReadFile(saved)
					require.NoError(t, readErr)
				},
			})
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: agentName, ReviewType: tt.reviewType,
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)

			tc.Pool.processJob(testWorkerID, claimed)

			tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			if tt.evidence {
				assert.Equal(t, "%PDF\x00committed", string(savedData))
				assert.NoFileExists(t, saved, "evidence is removed after the attempt")
			}
		})
	}
}
