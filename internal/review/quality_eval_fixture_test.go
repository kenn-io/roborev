package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// qualityDataRoot is explicit when the same frozen harness is overlaid onto
// another product tree. It never resolves through runtime.Caller (-trimpath).
func qualityDataRoot() string {
	if root := os.Getenv("ROBOREV_QUALITY_HARNESS_DIR"); root != "" {
		return filepath.Join(root, "internal", "review", "testdata", "quality")
	}
	return filepath.Join("testdata", "quality")
}

func qualityFixture(mechanism, variant string) ([]byte, error) {
	if variant == "unchanged" || variant == "harmless" {
		source, err := qualityFixture(mechanism, "vulnerable")
		if variant == "harmless" {
			source = append([]byte("// Harmless documentation edit.\n"), source...)
		}
		return source, err
	}
	if !slices.Contains([]string{"invoice", "cursor"}, mechanism) || !slices.Contains([]string{"vulnerable", "fixed", "safe"}, variant) {
		return nil, fmt.Errorf("unknown fixture %q/%q", mechanism, variant)
	}
	return os.ReadFile(filepath.Join(qualityDataRoot(), mechanism, variant, "fixture.go"))
}

func qualityHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// This executes the exact bytes reviewed by the model. Changing the repair to
// reject every tenant or skip the first greater cursor value fails a control.
func TestQualityFixtureBehavior(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture.test\n\ngo 1.27.3\n"), 0o600))
	var imports, calls strings.Builder
	imports.WriteString("package main\nimport (\n \"encoding/json\"\n \"os\"\n")
	for _, mechanism := range []string{"invoice", "cursor"} {
		for _, variant := range []string{"vulnerable", "fixed", "safe", "unchanged", "harmless"} {
			source, err := qualityFixture(mechanism, variant)
			require.NoError(t, err)
			dir := filepath.Join(root, mechanism, variant)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "fixture.go"), source, 0o600))
			name := mechanism + variant
			fmt.Fprintf(&imports, "%s %q\n", name, "fixture.test/"+mechanism+"/"+variant)
			if mechanism == "invoice" {
				fmt.Fprintf(&calls, `{ invoices:=map[string]%s.Invoice{"owned":{Tenant:"owner",Amount:70}}
     own,ownOK:=%s.ReadInvoice(invoices,"owner","owned")
     foreign,foreignOK:=%s.ReadInvoice(invoices,"other","owned")
     missing,missingOK:=%s.ReadInvoice(invoices,"owner","missing")
     results[%q]=map[string]any{"Own":own,"OwnOK":ownOK,"Foreign":foreign,"ForeignOK":foreignOK,"Missing":missing,"MissingOK":missingOK}
    }
`, name, name, name, name, mechanism+"/"+variant)
			} else {
				fmt.Fprintf(&calls, `results[%q]=map[string]any{"Equality":%s.After([]int{1,2,3,4},2),"First":%s.After([]int{1,2,3,4},0),"Empty":len(%s.After(nil,2)),"Exhausted":len(%s.After([]int{1,2,3,4},5))}
`, mechanism+"/"+variant, name, name, name, name)
			}
		}
	}
	probe := imports.String() + ")\nfunc main(){results:=map[string]any{}\n" + calls.String() + "json.NewEncoder(os.Stdout).Encode(results)\n}\n"
	require.NoError(t, os.Mkdir(filepath.Join(root, "probe"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "probe", "main.go"), []byte(probe), 0o600))
	// One focused child compilation executes all ten fixture packages. The child
	// removes native account overrides; assertions stay in this testify test.
	cmd := exec.CommandContext(t.Context(), "go", "run", "./probe")
	cmd.Dir = root
	cmd.Env = qualityOfflineEnv(os.Environ())
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	var observed map[string]struct {
		Own, Foreign, Missing       int
		OwnOK, ForeignOK, MissingOK bool
		Equality, First             []int
		Empty, Exhausted            int
	}
	require.NoError(t, json.Unmarshal(output, &observed))
	require.Len(t, observed, 10)
	for name, got := range observed {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			vulnerable := strings.HasSuffix(name, "/vulnerable") || strings.HasSuffix(name, "/unchanged") || strings.HasSuffix(name, "/harmless")
			if strings.HasPrefix(name, "invoice/") {
				assert.Equal(70, got.Own)
				assert.True(got.OwnOK)
				assert.Zero(got.Missing)
				assert.False(got.MissingOK)
				if vulnerable {
					assert.Equal(70, got.Foreign)
					assert.True(got.ForeignOK)
				} else {
					assert.Zero(got.Foreign)
					assert.False(got.ForeignOK)
				}
			} else {
				want := []int{3, 4}
				if vulnerable {
					want = []int{2, 3, 4}
				}
				assert.Equal(want, got.Equality)
				assert.Equal([]int{1, 2, 3, 4}, got.First)
				assert.Zero(got.Empty)
				assert.Zero(got.Exhausted)
			}
		})
	}
}

func qualityOfflineEnv(env []string) []string {
	var out []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "GOMAXPROCS", "GOFLAGS", "GOWORK":
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GOMAXPROCS=2", "GOFLAGS=-p=2", "GOWORK=off")
}

func qualityCommand(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(qualityOfflineEnv(os.Environ()), "GIT_AUTHOR_DATE=2024-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2024-01-01T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func TestQualityOfflineChildRemovesNativeOverrides(t *testing.T) {
	got := qualityOfflineEnv([]string{"CODEX_HOME=private", "CLAUDE_CONFIG_DIR=private", "PI_CODING_AGENT_DIR=private", "GOMAXPROCS=8", "GOFLAGS=-p=8", "KEEP=yes"})
	assert.ElementsMatch(t, []string{"KEEP=yes", "GOMAXPROCS=2", "GOFLAGS=-p=2", "GOWORK=off"}, got)
}

type qualityFrozenCorpus struct{ Files map[string][]byte }

func qualityCorpusPaths() []string {
	paths := []string{"rubric.json"}
	for _, mechanism := range []string{"invoice", "cursor"} {
		for _, variant := range []string{"vulnerable", "fixed", "safe"} {
			paths = append(paths, mechanism+"/"+variant+"/fixture.go")
		}
	}
	return paths
}

// Read and retain committed bytes. Reject dirty inputs explicitly, then avoid
// all subsequent worktree reads so later mutations cannot change a case/rubric.
func qualityLoadFrozenCorpus(ctx context.Context, root, head string) (qualityFrozenCorpus, error) {
	corpus := qualityFrozenCorpus{Files: map[string][]byte{}}
	if head == "" {
		return corpus, fmt.Errorf("frozen corpus requires a committed harness revision")
	}
	for _, name := range qualityCorpusPaths() {
		relative := "internal/review/testdata/quality/" + name
		cmd := exec.CommandContext(ctx, "git", "show", head+":"+relative)
		cmd.Dir = root
		committed, err := cmd.Output()
		if err != nil {
			return corpus, fmt.Errorf("read committed corpus %s: %w", name, err)
		}
		current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return corpus, fmt.Errorf("read corpus checkout %s: %w", name, err)
		}
		if !bytes.Equal(committed, current) {
			return corpus, fmt.Errorf("runtime corpus %s differs from frozen harness revision", name)
		}
		corpus.Files[name] = committed
	}
	return corpus, nil
}

func (corpus qualityFrozenCorpus) Fixture(mechanism, variant string) ([]byte, error) {
	if variant == "unchanged" || variant == "harmless" {
		source, err := corpus.Fixture(mechanism, "vulnerable")
		if variant == "harmless" {
			source = append([]byte("// Harmless documentation edit.\n"), source...)
		}
		return source, err
	}
	data, ok := corpus.Files[mechanism+"/"+variant+"/fixture.go"]
	if !ok {
		return nil, fmt.Errorf("unknown frozen fixture %q/%q", mechanism, variant)
	}
	return slices.Clone(data), nil
}

func qualityFrozenCorpusFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range qualityCorpusPaths() {
		data, err := os.ReadFile(filepath.Join(qualityDataRoot(), name))
		require.NoError(t, err)
		p := filepath.Join(root, "internal", "review", "testdata", "quality", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, data, 0o600))
	}
	for _, args := range [][]string{{"git", "init", "-q"}, {"git", "config", "user.name", "Fixture Reviewer"}, {"git", "config", "user.email", "reviewer@example.test"}, {"git", "add", "internal/review/testdata/quality"}, {"git", "commit", "-q", "-m", "test: freeze synthetic corpus"}} {
		_, err := qualityCommand(t.Context(), root, args...)
		require.NoError(t, err)
	}
	head, err := qualityCommand(t.Context(), root, "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	return root, head
}

func TestQualityFrozenCorpusRejectsTamperedFixtureAndRubric(t *testing.T) {
	for _, name := range []string{"cursor/fixed/fixture.go", "rubric.json"} {
		t.Run(strings.ReplaceAll(name, "/", "-"), func(t *testing.T) {
			root, head := qualityFrozenCorpusFixture(t)
			_, err := qualityLoadFrozenCorpus(t.Context(), root, head)
			require.NoError(t, err)
			target := filepath.Join(root, "internal", "review", "testdata", "quality", name)
			changed := []byte(`{"adjudication":"changed rubric"}`)
			if name != "rubric.json" {
				changed, err = os.ReadFile(target)
				require.NoError(t, err)
				changed = []byte(strings.ReplaceAll(string(changed), "> cursor", ">= cursor"))
			}
			require.NoError(t, os.WriteFile(target, changed, 0o600))
			_, err = qualityLoadFrozenCorpus(t.Context(), root, head)
			require.Error(t, err, "mutable runtime inputs must not inherit the committed harness identity")
		})
	}
}

func TestQualityFrozenCorpusKeepsCommittedSnapshot(t *testing.T) {
	root, head := qualityFrozenCorpusFixture(t)
	snapshot, err := qualityLoadFrozenCorpus(t.Context(), root, head)
	require.NoError(t, err)
	expected, err := snapshot.Fixture("cursor", "fixed")
	require.NoError(t, err)
	expected = bytes.Clone(expected)
	require.NoError(t, os.WriteFile(filepath.Join(root, "internal", "review", "testdata", "quality", "cursor", "fixed", "fixture.go"), []byte("changed after snapshot"), 0o600))
	observed, err := snapshot.Fixture("cursor", "fixed")
	require.NoError(t, err)
	assert.Equal(t, expected, observed)
	observed[0] = 'X'
	again, err := snapshot.Fixture("cursor", "fixed")
	require.NoError(t, err)
	assert.Equal(t, expected, again, "consumers must not mutate another fixture read")
}

// Exercise Git's real checkout conversion. The unpinned control proves that
// autocrlf is active while the strict corpus byte check still authenticates.
func TestQualityFrozenCorpusAcceptsAutocrlfCheckout(t *testing.T) {
	root, _ := qualityFrozenCorpusFixture(t)
	attrs, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitattributes"), attrs, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "checkout-control.txt"), []byte("checkout control\n"), 0o600))
	for _, args := range [][]string{
		{"git", "add", ".gitattributes", "checkout-control.txt"},
		{"git", "commit", "-q", "-m", "test: preserve checkout byte identity"},
		{"git", "config", "core.autocrlf", "true"},
	} {
		_, err := qualityCommand(t.Context(), root, args...)
		require.NoError(t, err)
	}
	for _, name := range qualityCorpusPaths() {
		require.NoError(t, os.Remove(filepath.Join(root, "internal", "review", "testdata", "quality", filepath.FromSlash(name))))
	}
	require.NoError(t, os.Remove(filepath.Join(root, "checkout-control.txt")))
	_, err = qualityCommand(t.Context(), root, "git", "checkout-index", "--all", "--force")
	require.NoError(t, err)
	control, err := os.ReadFile(filepath.Join(root, "checkout-control.txt"))
	require.NoError(t, err)
	assert.Contains(t, string(control), "\r\n", "the checkout must actually apply autocrlf to unpinned text")
	head, err := qualityCommand(t.Context(), root, "git", "rev-parse", "HEAD")
	require.NoError(t, err)
	corpus, err := qualityLoadFrozenCorpus(t.Context(), root, head)
	require.NoError(t, err, "clean checkout bytes must authenticate against the committed corpus")
	assert.Len(t, corpus.Files, len(qualityCorpusPaths()))
}
