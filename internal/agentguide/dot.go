// Package agentguide validates the small DOT subset used in agent guidance.
// It deliberately accepts fewer constructs than Graphviz, so flows have one
// entry, explicit decisions, and reachable actions.
package agentguide

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Graph struct {
	Name  string
	Nodes []Node
	Edges []Edge
}

type (
	Node struct{ ID, Shape, Label string }
	Edge struct{ From, To, Label string }
)

func (g Graph) Node(id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (g Graph) Out(id string) []Edge {
	var edges []Edge
	for _, e := range g.Edges {
		if e.From == id {
			edges = append(edges, e)
		}
	}
	return edges
}

func FenceDOT(body string) string { return "~~~dot\n" + strings.TrimSpace(body) + "\n~~~" }

// ExtractDOT returns the body of exactly one complete tilde DOT fence.
func ExtractDOT(doc string) (string, error) {
	var body strings.Builder
	count, inside := 0, false
	for line := range strings.SplitSeq(doc, "\n") {
		switch strings.TrimSuffix(line, "\r") {
		case "~~~dot":
			count++
			if count > 1 {
				return "", errors.New("fence: expected exactly one DOT block")
			}
			inside = true
		case "~~~":
			if inside {
				inside = false
			}
		default:
			if inside {
				body.WriteString(line)
				body.WriteByte('\n')
			}
		}
	}
	if count != 1 || inside {
		return "", errors.New("fence: missing or unclosed DOT block")
	}
	return strings.TrimSpace(body.String()), nil
}

var (
	graphHeader = regexp.MustCompile(`^digraph[ \t]+(roborev_[a-z][a-z0-9_]*)[ \t]*\{$`)
	identifier  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// LintDOT checks the guidance grammar and graph structure. Scanning and graph
// validation take linear time in the input and its nodes and edges.
func LintDOT(doc string) (Graph, error) {
	body, err := ExtractDOT(doc)
	if err != nil {
		return Graph{}, err
	}
	if strings.Contains(body, "{{") {
		return Graph{}, errors.New("template: {{ is not allowed in DOT")
	}
	header, rest, ok := strings.Cut(body, "\n")
	match := graphHeader.FindStringSubmatch(strings.TrimSpace(header))
	if !ok || match == nil {
		return Graph{}, errors.New("graph name/header: expected digraph roborev_<name> {")
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasSuffix(rest, "}") {
		return Graph{}, errors.New("graph: missing closing }")
	}
	statements, err := splitStatements(strings.TrimSpace(strings.TrimSuffix(rest, "}")))
	if err != nil {
		return Graph{}, err
	}
	g := Graph{Name: match[1]}
	nodes := map[string]Node{}
	for i, statement := range statements {
		subject, attrs, err := parseStatement(statement)
		if err != nil {
			return Graph{}, fmt.Errorf("statement %d: %w", i+1, err)
		}
		if i == 0 {
			if subject != "node" || len(attrs) != 1 || attrs["shape"] != "box" {
				return Graph{}, errors.New("first statement: expected node [shape=box]")
			}
			continue
		}
		if strings.Contains(subject, "->") {
			for key := range attrs {
				if key != "label" {
					return Graph{}, fmt.Errorf("edge attribute %q is not supported", key)
				}
			}
			ids := strings.Split(subject, "->")
			for j := range ids {
				ids[j] = strings.TrimSpace(ids[j])
				if _, ok := nodes[ids[j]]; !ok {
					return Graph{}, fmt.Errorf("edge endpoint %q must be declared before its edge", ids[j])
				}
			}
			for j := 1; j < len(ids); j++ {
				g.Edges = append(g.Edges, Edge{From: ids[j-1], To: ids[j], Label: attrs["label"]})
			}
			continue
		}
		if !identifier.MatchString(subject) || reservedID(subject) {
			return Graph{}, fmt.Errorf("statement %d: invalid node %q", i+1, subject)
		}
		if _, exists := nodes[subject]; exists {
			return Graph{}, fmt.Errorf("node %q: duplicate declaration", subject)
		}
		for key := range attrs {
			if key != "shape" && key != "label" {
				return Graph{}, fmt.Errorf("node %q: unsupported attribute %q", subject, key)
			}
		}
		shape, hasShape := attrs["shape"]
		if !hasShape {
			shape = "box"
		}
		if shape != "box" && shape != "diamond" && shape != "oval" {
			return Graph{}, fmt.Errorf("node %q: unsupported shape %q", subject, shape)
		}
		if strings.TrimSpace(attrs["label"]) == "" {
			return Graph{}, fmt.Errorf("node %q: missing label", subject)
		}
		n := Node{ID: subject, Shape: shape, Label: attrs["label"]}
		nodes[subject] = n
		g.Nodes = append(g.Nodes, n)
	}
	if err := validateGraph(g); err != nil {
		return Graph{}, err
	}
	return g, nil
}

func splitStatements(s string) ([]string, error) {
	var statements []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if quoted {
				i++
				if i >= len(s) || s[i] != 'n' {
					return nil, errors.New("label: only the \\n escape is allowed")
				}
			}
		case '"':
			quoted = !quoted
		case '\n', '\r':
			if quoted {
				return nil, errors.New("label: use \\n instead of literal newlines")
			}
		case ';':
			if !quoted {
				statement := strings.TrimSpace(s[start:i])
				if statement == "" {
					return nil, errors.New("statement: empty statement")
				}
				statements = append(statements, statement)
				start = i + 1
			}
		}
	}
	if quoted {
		return nil, errors.New("label: unterminated quote")
	}
	if strings.TrimSpace(s[start:]) != "" {
		return nil, errors.New("statement: missing final ;")
	}
	return statements, nil
}

func parseStatement(s string) (string, map[string]string, error) {
	subject, raw, hasAttrs := strings.Cut(s, "[")
	subject = strings.TrimSpace(subject)
	attrs := map[string]string{}
	if !hasAttrs {
		if !strings.Contains(subject, "->") {
			return "", nil, fmt.Errorf("unsupported statement %q", s)
		}
		return subject, attrs, nil
	}
	if !strings.HasSuffix(raw, "]") {
		return "", nil, fmt.Errorf("unsupported statement %q", s)
	}
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "]"))
	for raw != "" {
		eq := strings.IndexByte(raw, '=')
		if eq < 0 {
			return "", nil, fmt.Errorf("attribute missing = in %q", raw)
		}
		key := strings.TrimSpace(raw[:eq])
		if !identifier.MatchString(key) {
			return "", nil, fmt.Errorf("invalid attribute %q", key)
		}
		if _, exists := attrs[key]; exists {
			return "", nil, fmt.Errorf("duplicate attribute %q", key)
		}
		raw = strings.TrimSpace(raw[eq+1:])
		value := ""
		if strings.HasPrefix(raw, `"`) {
			end := strings.IndexByte(raw[1:], '"')
			if end < 0 {
				return "", nil, errors.New("label: unterminated quote")
			}
			value, raw = raw[1:end+1], raw[end+2:]
		} else {
			end := strings.IndexAny(raw, " \t\r\n,")
			if end < 0 {
				end = len(raw)
			}
			value, raw = raw[:end], raw[end:]
			if key == "label" {
				return "", nil, errors.New("label must be double-quoted")
			}
		}
		if raw != "" && !strings.ContainsRune(" \t\r\n,", rune(raw[0])) {
			return "", nil, errors.New("attribute: missing separator")
		}
		if key == "label" && (!utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) >= 0) {
			return "", nil, errors.New("label: control characters are not allowed")
		}
		if value == "" {
			return "", nil, fmt.Errorf("attribute %q: empty value", key)
		}
		attrs[key] = value
		raw = strings.TrimLeft(raw, " \t\r\n,")
	}
	return subject, attrs, nil
}

func validateGraph(g Graph) error {
	if len(g.Nodes) == 0 {
		return errors.New("entry: no nodes declared")
	}
	incoming := map[string]int{}
	outgoing := map[string][]Edge{}
	for _, e := range g.Edges {
		incoming[e.To]++
		outgoing[e.From] = append(outgoing[e.From], e)
	}
	entry, exits := g.Nodes[0], 0
	for _, n := range g.Nodes {
		if n.Shape == "oval" && len(outgoing[n.ID]) == 0 {
			exits++
		}
	}
	if exits == 0 {
		return errors.New("exit: expected an oval node with no outgoing edges")
	}
	if entry.Shape != "oval" || incoming[entry.ID] != 0 {
		return fmt.Errorf("entry %q: first node must be oval with no incoming edges", entry.ID)
	}
	for _, n := range g.Nodes {
		edges := outgoing[n.ID]
		if len(edges)+incoming[n.ID] == 0 {
			return fmt.Errorf("node %q: not on any edge", n.ID)
		}
		if n.ID != entry.ID && incoming[n.ID] == 0 {
			return fmt.Errorf("entry: node %q is another entry", n.ID)
		}
		if n.Shape == "oval" && n.ID != entry.ID && len(edges) > 0 {
			return fmt.Errorf("node %q: oval must be entry or exit", n.ID)
		}
		if n.Shape == "diamond" {
			if !strings.HasSuffix(strings.TrimSpace(n.Label), "?") {
				return fmt.Errorf("node %q: decision label must end in ?", n.ID)
			}
			if len(edges) < 2 {
				return fmt.Errorf("node %q: decision must branch to at least two edges", n.ID)
			}
			for _, e := range edges {
				if strings.TrimSpace(e.Label) == "" {
					return fmt.Errorf("node %q: decision edge needs a label", n.ID)
				}
			}
		} else if len(edges) > 1 {
			return fmt.Errorf("node %q: only diamonds may branch", n.ID)
		}
	}
	seen := map[string]bool{entry.ID: true}
	queue := []string{entry.ID}
	for i := 0; i < len(queue); i++ {
		for _, e := range outgoing[queue[i]] {
			if !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	for _, n := range g.Nodes {
		if !seen[n.ID] {
			return fmt.Errorf("node %q: unreachable from entry", n.ID)
		}
	}
	return nil
}

var ErrGraphvizUnavailable = errors.New("graphviz dot not found on PATH")

func CanonWithGraphviz(ctx context.Context, dot string) error {
	path, err := exec.LookPath("dot")
	if err != nil {
		return ErrGraphvizUnavailable
	}
	cmd := exec.CommandContext(ctx, path, "-Tcanon")
	cmd.Stdin = strings.NewReader(dot)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("graphviz: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func reservedID(id string) bool {
	switch id {
	case "node", "edge", "graph", "digraph", "strict", "subgraph":
		return true
	}
	return false
}
