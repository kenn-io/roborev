package review

import (
	"context"
	"encoding/json"
	"errors"
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

type fileSynthesisAgent struct {
	commonMockAgent
	read func(string, string) (string, error)
}

func (a *fileSynthesisAgent) Name() string        { return "test" }
func (a *fileSynthesisAgent) CommandLine() string { return "test" }
func (a *fileSynthesisAgent) Review(_ context.Context, repo, _, prompt string, _ io.Writer) (string, error) {
	return a.read(repo, prompt)
}

type fileSchemaSynthesisAgent struct{ *fileSynthesisAgent }

func (a *fileSchemaSynthesisAgent) ClassifyWithSchema(context.Context, string, string, string, json.RawMessage, io.Writer) (json.RawMessage, error) {
	return nil, errors.New("file inputs cannot use classifier tools")
}

func (a *fileSchemaSynthesisAgent) ReviewWithSchema(_ context.Context, repo, _, prompt string, schema json.RawMessage, _ io.Writer) (json.RawMessage, error) {
	result, err := a.read(repo, prompt)
	return json.RawMessage(result), err
}

func TestSynthesisReadsCompleteReviewsFromFiles(t *testing.T) {
	for _, schema := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(strconv.FormatBool(schema)+"/failure="+strconv.FormatBool(fail), func(t *testing.T) {
				repo := t.TempDir()
				testutil.InitTestGitRepo(t, repo)
				reviews := []ReviewResult{
					{Status: ResultDone, Output: strings.Repeat("first review\n", 1500) + "finding at end one"},
					{Status: ResultDone, Output: strings.Repeat("second review\n", 1500) + "finding at end two"},
				}
				var files []string
				a := &fileSynthesisAgent{}
				a.self = a
				a.read = func(gotRepo, prompt string) (string, error) {
					assert.Equal(t, repo, gotRepo)
					assert.Less(t, len(prompt), 4096)
					assert.Contains(t, prompt, "Read the complete task prompt")
					found, err := filepath.Glob(filepath.Join(repo, ".roborev", "*", "prompt.md"))
					require.NoError(t, err)
					files = found
					require.Len(t, files, 1)
					content, err := os.ReadFile(files[0])
					require.NoError(t, err)
					assert.Equal(t, BuildSynthesisPrompt(reviews, ""), string(content))

					if fail {
						return "", errors.New("agent failed")
					}
					return `{"schema_version":2,"summary":"combined","verdict":"fail","findings":[{"severity":"medium","problem":"finding at end two","fix":"fix","location":null,"sources":[2]}]}`, nil
				}
				var selected agent.Agent = a
				if schema {
					selected = &fileSchemaSynthesisAgent{a}
				}
				doc, err := RunSynthesisAgent(context.Background(), selected, reviews, BuildSynthesisPrompt(reviews, ""), "", nil, SynthesisHooks{
					ConfigRepoPath: repo,
					GlobalConfig:   &config.Config{DefaultMaxPromptSize: 4096},
					Checkout:       func() (SynthesisCheckout, error) { return SynthesisCheckout{RepoPath: repo}, nil },
				})
				if fail {
					require.ErrorContains(t, err, "agent failed")
				} else {
					require.NoError(t, err)
					require.Len(t, doc.Findings, 1)
					assert.Equal(t, []int{2}, doc.Findings[0].Sources)
				}
				require.Len(t, files, 1)
				for _, file := range files {
					_, err := os.Stat(file)
					require.ErrorIs(t, err, os.ErrNotExist)
				}
			})
		}
	}
}
