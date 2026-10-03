package goalreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/kata"
)

type Edge struct {
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

type Snapshot struct {
	Source    string       `json:"source"`
	Stage     string       `json:"stage"`
	Project   string       `json:"project"`
	Artifacts []Artifact   `json:"artifacts"`
	Issues    []kata.Issue `json:"issues"`
	Edges     []Edge       `json:"edges"`
}

// Capture bounds filesystem retries. It detects artifact edits across the ledger
// read without claiming an atomic transaction spanning files and Kata.
func Capture(ctx context.Context, root string, selection Selection, client kata.Client) (Snapshot, error) {
	var last error
	for range 3 {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		artifacts, err := ResolveArtifacts(root, selection)
		if err != nil {
			last = err
			continue
		}
		snapshot := Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []Artifact{artifacts.Spec}, Issues: []kata.Issue{}, Edges: []Edge{}}
		if artifacts.Plan != nil {
			snapshot.Stage = "plan"
			snapshot.Artifacts = append(snapshot.Artifacts, *artifacts.Plan)
		}
		binding, err := client.Binding(ctx)
		if err != nil && !errors.Is(err, kata.ErrNoBinding) {
			return Snapshot{}, err
		}
		if err == nil {
			if binding.Project == "" {
				return Snapshot{}, fmt.Errorf("empty Kata project binding")
			}
			snapshot.Project = binding.Project
			snapshot.Issues, err = client.List(ctx, kata.ListOpts{Status: "open", Unlimited: true})
			if err != nil {
				return Snapshot{}, err
			}
			for _, issue := range snapshot.Issues {
				if issue.ShortID == "" || issue.Status != "open" {
					return Snapshot{}, fmt.Errorf("invalid open Kata issue")
				}
				source := snapshot.ref(issue.ShortID)
				peerRef := func(peer kata.LinkPeer) string {
					if peer.QualifiedID != "" {
						return peer.QualifiedID
					}
					if peer.Project != "" {
						return peer.Project + "#" + peer.ShortID
					}
					return snapshot.ref(peer.ShortID)
				}
				if issue.Parent != nil {
					snapshot.Edges = append(snapshot.Edges, Edge{Type: "parent", From: source, To: peerRef(*issue.Parent)})
				}
				for _, peer := range issue.Blocks {
					snapshot.Edges = append(snapshot.Edges, Edge{Type: "blocks", From: source, To: peerRef(peer)})
				}
				for _, peer := range issue.BlockedBy {
					snapshot.Edges = append(snapshot.Edges, Edge{Type: "blocks", From: peerRef(peer), To: source})
				}
				for _, peer := range issue.Related {
					snapshot.Edges = append(snapshot.Edges, Edge{Type: "related", From: source, To: peerRef(peer)})
				}
			}
		}
		again, err := ResolveArtifacts(root, selection)
		if err != nil {
			last = err
			continue
		}
		if artifacts.Spec != again.Spec || (artifacts.Plan == nil) != (again.Plan == nil) || (artifacts.Plan != nil && *artifacts.Plan != *again.Plan) {
			last = fmt.Errorf("superpowers artifacts changed during capture")
			continue
		}
		return snapshot.canonical(), nil
	}
	return Snapshot{}, fmt.Errorf("capture superpowers artifacts: %w", last)
}

func (s Snapshot) ref(id string) string {
	if strings.Contains(id, "#") {
		return id
	}
	for _, issue := range s.Issues {
		if issue.ShortID == id && issue.QualifiedID != "" {
			return issue.QualifiedID
		}
	}
	if s.Project == "" {
		return id
	}
	return s.Project + "#" + id
}

func (s Snapshot) clone() Snapshot {
	s.Artifacts = slices.Clone(s.Artifacts)
	s.Issues = slices.Clone(s.Issues)
	for i := range s.Issues {
		s.Issues[i].Labels = slices.Clone(s.Issues[i].Labels)
		s.Issues[i].Blocks = slices.Clone(s.Issues[i].Blocks)
		s.Issues[i].BlockedBy = slices.Clone(s.Issues[i].BlockedBy)
		s.Issues[i].Related = slices.Clone(s.Issues[i].Related)
		if s.Issues[i].Parent != nil {
			parent := *s.Issues[i].Parent
			s.Issues[i].Parent = &parent
		}
	}
	s.Edges = slices.Clone(s.Edges)
	return s
}

func (s Snapshot) canonical() Snapshot {
	s = s.clone()
	for i := range s.Issues {
		issue := &s.Issues[i]
		slices.Sort(issue.Labels)
		issue.Labels = slices.Compact(issue.Labels)
		if issue.Labels == nil {
			issue.Labels = []string{}
		}
		issue.QualifiedID = s.ref(issue.ShortID)
		// Relations have one canonical representation in Edges.
		issue.Parent = nil
		issue.Blocks = nil
		issue.BlockedBy = nil
		issue.Related = nil
	}
	slices.SortFunc(s.Issues, func(a, b kata.Issue) int { return strings.Compare(a.QualifiedID, b.QualifiedID) })
	slices.SortFunc(s.Artifacts, func(a, b Artifact) int { return strings.Compare(a.Kind, b.Kind) })
	for i := range s.Edges {
		edge := &s.Edges[i]
		edge.From = s.ref(edge.From)
		edge.To = s.ref(edge.To)
		if edge.Type == "related" && edge.From > edge.To {
			edge.From, edge.To = edge.To, edge.From
		}
	}
	slices.SortFunc(s.Edges, func(a, b Edge) int {
		if n := strings.Compare(a.Type, b.Type); n != 0 {
			return n
		}
		if n := strings.Compare(a.From, b.From); n != 0 {
			return n
		}
		return strings.Compare(a.To, b.To)
	})
	s.Edges = slices.Compact(s.Edges)
	if s.Artifacts == nil {
		s.Artifacts = []Artifact{}
	}
	if s.Issues == nil {
		s.Issues = []kata.Issue{}
	}
	if s.Edges == nil {
		s.Edges = []Edge{}
	}
	return s
}

func (s Snapshot) ID() string {
	data, _ := json.Marshal(s.canonical())
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var taskCheckbox = regexp.MustCompile(`^([ \t]*[-*+]\s+\[)[ xX](\])`)

func normalizeCheckboxes(content string) string {
	lines := strings.Split(content, "\n")
	proseLines(content, func(line int, _ string) bool {
		lines[line-1] = taskCheckbox.ReplaceAllString(lines[line-1], "${1} ${2}")
		return true
	})
	return strings.Join(lines, "\n")
}

func (s Snapshot) WatchID(watch []string) string {
	s = s.clone()
	if slices.Contains(watch, "goal") {
		for i := range s.Artifacts {
			s.Artifacts[i].Content = normalizeCheckboxes(s.Artifacts[i].Content)
		}
	} else {
		s.Artifacts = nil
		s.Source = ""
		s.Stage = ""
	}
	if !slices.Contains(watch, "kata_graph") {
		s.Project = ""
		s.Issues = nil
		s.Edges = nil
	} else {
		for i := range s.Issues {
			s.Issues[i].Body = normalizeCheckboxes(s.Issues[i].Body)
		}
	}
	return s.ID()
}

type Candidate struct {
	ShortID string          `json:"short_id,omitempty"`
	Title   string          `json:"title"`
	Body    string          `json:"body"`
	Labels  []string        `json:"labels,omitempty"`
	Links   []CandidateLink `json:"links,omitempty"`
}

// MarshalJSON preserves the distinction between omitted and explicitly empty
// slices when a client sends an edit through the candidate gate.
func (c Candidate) MarshalJSON() ([]byte, error) {
	type wire struct {
		ShortID string           `json:"short_id,omitempty"`
		Title   string           `json:"title"`
		Body    string           `json:"body"`
		Labels  *[]string        `json:"labels,omitempty"`
		Links   *[]CandidateLink `json:"links,omitempty"`
	}
	value := wire{ShortID: c.ShortID, Title: c.Title, Body: c.Body}
	if c.Labels != nil {
		value.Labels = &c.Labels
	}
	if c.Links != nil {
		value.Links = &c.Links
	}
	return json.Marshal(value)
}

type CandidateLink struct {
	Type     string `json:"type"`
	ToRef    string `json:"to_ref"`
	Incoming bool   `json:"incoming,omitempty"`
}

// WithCandidate simulates a create/edit. Nil slices preserve existing values;
// non-nil empty slices clear them. No changes are written to Kata.
func (s Snapshot) WithCandidate(candidate Candidate) (Snapshot, error) {
	if strings.TrimSpace(candidate.Title) == "" || strings.TrimSpace(candidate.Body) == "" {
		return Snapshot{}, fmt.Errorf("candidate title and body are required")
	}
	result := s.canonical()
	index := -1
	if candidate.ShortID != "" {
		for i, issue := range result.Issues {
			if issue.ShortID == candidate.ShortID {
				index = i
				break
			}
		}
		if index < 0 {
			return Snapshot{}, fmt.Errorf("candidate edit must name an open Kata short ID")
		}
	} else {
		for _, issue := range result.Issues {
			if issue.ShortID == "proposed" {
				return Snapshot{}, fmt.Errorf("reserved proposed ID already exists")
			}
		}
		result.Issues = append(result.Issues, kata.Issue{ShortID: "proposed", Status: "open"})
		index = len(result.Issues) - 1
	}
	issue := &result.Issues[index]
	issue.Title = candidate.Title
	issue.Body = candidate.Body
	if candidate.Labels != nil {
		issue.Labels = slices.Clone(candidate.Labels)
	}
	if candidate.Links != nil {
		source := result.ref(issue.ShortID)
		known := map[string]bool{}
		for _, existing := range result.Issues {
			known[result.ref(existing.ShortID)] = true
		}
		result.Edges = slices.DeleteFunc(result.Edges, func(edge Edge) bool { return edge.From == source || edge.To == source })
		parents := 0
		for _, link := range candidate.Links {
			target := result.ref(link.ToRef)
			if target == source || !known[target] {
				return Snapshot{}, fmt.Errorf("unknown or self candidate link target: %s", link.ToRef)
			}
			switch link.Type {
			case "parent":
				parents++
				if parents > 1 {
					return Snapshot{}, fmt.Errorf("candidate can have only one parent")
				}
			case "blocks", "related":
			default:
				return Snapshot{}, fmt.Errorf("invalid candidate link type: %s", link.Type)
			}
			if link.Incoming && link.Type != "blocks" {
				return Snapshot{}, fmt.Errorf("incoming is supported only for blocks")
			}
			edge := Edge{Type: link.Type, From: source, To: target}
			if link.Incoming {
				edge.From, edge.To = edge.To, edge.From
			}
			result.Edges = append(result.Edges, edge)
		}
	}
	return result.canonical(), nil
}
