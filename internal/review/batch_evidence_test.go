package review

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

type evidenceBatchAgent struct {
	mockAgent
	prompt string
	files  []string
	bytes  []byte
}

func (a *evidenceBatchAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *evidenceBatchAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *evidenceBatchAgent) WithModel(string) agent.Agent                   { return a }
func (a *evidenceBatchAgent) Review(ctx context.Context, repo, ref, text string, output io.Writer) (string, error) {
	a.prompt = text
	files, err := filepath.Glob(filepath.Join(repo, ".roborev", "roborev-snapshot-*", "*.pdf"))
	if err != nil {
		return "", err
	}
	for _, file := range files {
		// A concurrent member may own a file. The bound prompt identifies this attempt.
		if !strings.Contains(text, strconv.Quote(file)) {
			continue
		}
		a.files = append(a.files, file)
		a.bytes, err = os.ReadFile(file)
		if err != nil {
			return "", err
		}
	}
	return a.mockAgent.Review(ctx, repo, ref, text, output)
}

func (a *evidenceBatchAgent) ReviewWithSchema(ctx context.Context, repo, ref, text string, _ jsontext.Value, out io.Writer) (jsontext.Value, error) {
	raw, err := a.Review(ctx, repo, ref, text, out)
	return jsontext.Value(raw), err
}

func TestRunBatchEvidenceOnlyForEnabledRubric(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			data := []byte("%PDF-1.7\n\x00draft\n")
			repo.CommitFile("draft.pdf", string(data), "draft")
			repo.CommitFile("rubric.md", "Check the draft.", "rubric")
			enabled := &evidenceBatchAgent{name: "evidence", output: "No issues found."}
			plain := &evidenceBatchAgent{name: "plain", output: "No issues found."}
			if fail {
				enabled.err = errors.New("synthetic agent failure")
			}
			cfg := BatchConfig{
				RepoPath:    repo.Path(),
				GitRef:      repo.RevParse("HEAD~1"),
				Agents:      []string{""},
				ReviewTypes: []string{"citations", "clarity"},
				RepoConfig: &config.RepoConfig{Review: config.ReviewConfig{Types: map[string]config.ReviewTypeSpec{
					"citations": {Template: "rubric.md", Agent: "evidence", BinaryEvidence: true},
					"clarity":   {Template: "rubric.md", Agent: "plain"},
				}}}, AgentRegistry: map[string]agent.Agent{"evidence": enabled, "plain": plain},
			}
			results := RunBatch(context.Background(), cfg)
			require.Len(t, results, 2)
			require.Equal(t, ResultDone, getResultByType(t, results, "clarity").Status, "%+v", results)
			assert := assert.New(t)
			assert.Contains(enabled.prompt, "## Committed review evidence")
			assert.NotContains(plain.prompt, "## Committed review evidence")
			require.Len(t, enabled.files, 1)
			assert.Equal(data, enabled.bytes)
			assert.Empty(plain.files)
			for _, file := range enabled.files {
				_, err := os.Stat(file)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			if fail {
				assert.Equal(ResultFailed, getResultByType(t, results, "citations").Status)
			} else {
				assert.Equal(ResultDone, getResultByType(t, results, "citations").Status)
			}
		})
	}
}
