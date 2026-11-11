package agentguide

import (
	"errors"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		"subgraph":          {"node [shape=box];", "node [shape=box]; subgraph x {};", "unsupported statement"},
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
	property := func(parts []bool) bool {
		var ws strings.Builder
		ws.WriteByte(' ')
		for _, tab := range parts {
			if tab {
				ws.WriteByte('\t')
			} else {
				ws.WriteByte(' ')
			}
		}
		doc := strings.ReplaceAll(validDoc, " [", ws.String()+"[")
		graph, err := LintDOT(doc)
		require.NoError(t, err, "whitespace=%q", ws.String())
		n, ok := graph.Node("act")
		require.True(t, ok)
		return assert.Equal(t, "roborev_sample", graph.Name) &&
			assert.Equal(t, "Act  on it", n.Label)
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}

func TestFenceDOTRoundTripProperty(t *testing.T) {
	property := func(body string) bool {
		// Fence extraction is exact for bodies without fence delimiter lines.
		if strings.Contains(body, "~~~") {
			return true
		}
		got, err := ExtractDOT(FenceDOT(body))
		require.NoError(t, err, "body=%q", body)
		return assert.Equal(t, strings.TrimSpace(body), got)
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
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
	property := func(label string, invalid bool, control uint8) bool {
		// Keep the original domain while producing both valid labels and labels
		// containing a forbidden control character on every QuickCheck run.
		label = strings.Map(func(r rune) rune {
			if r < 32 || r == 127 || r == '"' || r == '\\' {
				return 'a'
			}
			return r
		}, label)
		label = strings.ReplaceAll(strings.ReplaceAll(label, "{{", "aa"), "~~~", "aaa")
		if strings.TrimSpace(label) == "" {
			label = "a"
		}
		if invalid {
			controls := "\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f"
			label += string(controls[int(control)%len(controls)])
		}
		graph, err := LintDOT(strings.Replace(validDoc, "Act  on it", label, 1))
		if invalid {
			return assert.Error(t, err, "label=%q", label)
		}
		require.NoError(t, err, "label=%q", label)
		node, ok := graph.Node("act")
		require.True(t, ok)
		return assert.Equal(t, label, node.Label) &&
			assert.Equal(t, []Edge{{From: "act", To: "end"}}, graph.Out("act"))
	}
	require.NoError(t, quick.Check(property, &quick.Config{MaxCount: 1000}))
}
