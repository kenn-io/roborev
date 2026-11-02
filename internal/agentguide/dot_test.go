package agentguide

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

const validDoc = `~~~dot
digraph roborev_sample {
  node [shape=box];
  start [shape=oval label="Start"];
  ok [shape=diamond label="OK?"];
  act [label="Act  on it"];
  end [shape=oval label="Return"];
  start -> ok;
  ok -> act [label="yes"];
  ok -> end [label="no"];
  act -> end;
}
~~~`

func TestLintDOTAcceptsValidFlow(t *testing.T) {
	g, err := LintDOT(validDoc)
	require.NoError(t, err)
	assert.Equal(t, "roborev_sample", g.Name)
	n, ok := g.Node("ok")
	require.True(t, ok)
	assert.Equal(t, "diamond", n.Shape)
	assert.Len(t, g.Out("ok"), 2)
	n, ok = g.Node("act")
	require.True(t, ok)
	assert.Equal(t, "Act  on it", n.Label)
}

func TestLintDOTRejects(t *testing.T) {
	cases := map[string]struct{ old, replacement, want string }{
		"name":              {"roborev_sample", "Sample", "graph name"},
		"endpoint":          {"act -> end;", "act -> ghost;", "ghost"},
		"orphan":            {"end [", "lost [label=\"Lost\"];\nend [", "lost"},
		"unreachable":       {"end [", "lost [label=\"Lost\"]; lost -> lost;\nend [", "unreachable"},
		"decision label":    {`[label="no"]`, "", "label"},
		"question":          {"OK?", "OK", "?"},
		"box branches":      {"act -> end;", "act -> end; act -> ok;", "branch"},
		"no exit":           {"act -> end;", "act -> end; end -> ok;", "exit"},
		"template":          {"Act  on it", "{{.Job}}", "{{"},
		"subgraph":          {"node [shape=box];", "node [shape=box]; subgraph x {}", "statement"},
		"closing brace":     {"\n}\n~~~", "\n~~~", "}"},
		"duplicate":         {"end [", "act [label=\"Duplicate\"]; end [", "act"},
		"shape":             {"shape=diamond", "shape=egg", "shape"},
		"unknown attr":      {`label="Start"`, `color="red" label="Start"`, "attribute"},
		"escaped quote":     {"Act  on it", `Act \"now\"`, "escape"},
		"literal newline":   {"Act  on it", "Act\non it", "label"},
		"missing semicolon": {"act -> end;", "act -> end", ";"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LintDOT(strings.Replace(validDoc, tc.old, tc.replacement, 1))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	for _, doc := range []string{"digraph roborev_x {}", validDoc + "\n" + validDoc, "~~~dot\ndigraph roborev_x {"} {
		_, err := LintDOT(doc)
		require.ErrorContains(t, err, "fence")
	}
}

func TestCanonWithGraphviz(t *testing.T) {
	body, err := ExtractDOT(validDoc)
	require.NoError(t, err)
	err = CanonWithGraphviz(t.Context(), body)
	if errors.Is(err, ErrGraphvizUnavailable) {
		t.Skip("graphviz not installed")
	}
	require.NoError(t, err)
	require.Error(t, CanonWithGraphviz(t.Context(), "not dot"))
}

// Equivalent whitespace outside labels must preserve the graph, including labels.
func TestLintDOTWhitespaceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ws := " " + strings.Join(hegel.Draw(ht, hegel.Lists(hegel.SampledFrom([]string{" ", "\t"}))), "")
		doc := strings.ReplaceAll(validDoc, " [", ws+"[")
		graph, err := LintDOT(doc)
		require.NoError(ht, err)
		assert.Equal(ht, "roborev_sample", graph.Name)
		n, ok := graph.Node("act")
		require.True(ht, ok)
		assert.Equal(ht, "Act  on it", n.Label)
	})
}

func TestFenceDOTRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		body := hegel.Draw(ht, hegel.Text())
		// Fence extraction is exact for bodies without fence delimiter lines.
		if strings.Contains(body, "~~~") {
			ht.Skip()
		}
		got, err := ExtractDOT(FenceDOT(body))
		require.NoError(ht, err)
		assert.Equal(ht, strings.TrimSpace(body), got)
	})
}

func FuzzLintDOT(f *testing.F) {
	f.Add(validDoc)
	f.Add("~~~dot\ndigraph roborev_x {\n a [label=\"unterminated];\n}\n~~~")
	f.Add("~~~dot\n" + strings.Repeat("{", 1000) + "\n~~~")
	f.Fuzz(func(t *testing.T, doc string) { _, _ = LintDOT(doc) })
}

func TestLintDOTRejectsEmptyShapeAndReservedNodeIDs(t *testing.T) {
	for _, shape := range []string{"shape=", `shape=""`} {
		_, err := LintDOT(strings.Replace(validDoc, `act [label="Act  on it"]`, `act [label="Act  on it" `+shape+`]`, 1))
		require.Error(t, err, shape)
	}
	for _, id := range []string{"edge", "graph", "digraph", "strict", "subgraph"} {
		_, err := LintDOT(strings.ReplaceAll(validDoc, "act", id))
		require.Error(t, err, id)
	}
}

// Changing labels must never change graph connectivity, and parsed labels stay exact.
func TestLintDOTLabelProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		label := hegel.Draw(ht, hegel.Text())
		ht.Assume(strings.TrimSpace(label) != "" && !strings.ContainsAny(label, "\"\\\r\n\x00\t") && !strings.Contains(label, "{{") && !strings.Contains(label, "~~~"))
		graph, err := LintDOT(strings.Replace(validDoc, "Act  on it", label, 1))
		// Other control characters are outside the label grammar too.
		if strings.IndexFunc(label, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			require.Error(ht, err)
			return
		}
		require.NoError(ht, err)
		node, ok := graph.Node("act")
		require.True(ht, ok)
		assert.Equal(ht, label, node.Label)
		assert.Equal(ht, []Edge{{From: "act", To: "end"}}, graph.Out("act"))
	})
}
