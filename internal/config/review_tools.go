package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// ReviewToolPolicy restricts a review to read-only native tools plus the exact
// MCP tools it names. A nil policy keeps ordinary adapter behavior.
type ReviewToolPolicy struct {
	Mode string              `toml:"mode" json:"mode"`
	MCP  map[string][]string `toml:"mcp" json:"mcp"`
}

// ReviewSettings holds the resolved per-review settings frozen with panel and
// CI member config. Operator-only MCP settings never belong here.
type ReviewSettings struct {
	Tools          *ReviewToolPolicy `json:"tools,omitzero"`
	BinaryEvidence bool              `json:"binary_evidence,omitzero"`
}

var reviewToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (p *ReviewToolPolicy) Clone() *ReviewToolPolicy {
	if p == nil {
		return nil
	}
	result := &ReviewToolPolicy{Mode: p.Mode, MCP: maps.Clone(p.MCP)}
	for server, tools := range result.MCP {
		result.MCP[server] = slices.Clone(tools)
	}
	return result
}

// Names are exact identifiers. "__" is rejected because adapters join server
// and tool names with it.
func validReviewToolName(name string) bool {
	return reviewToolNamePattern.MatchString(name) && !strings.Contains(name, "__")
}

// Native names join server + "__" + tool, so a server ending in "_" or a tool
// starting with "_" could collide: archive_/lookup and archive/_lookup.
func validReviewMCPServerName(name string) bool {
	return validReviewToolName(name) && !strings.HasSuffix(name, "_")
}

func validReviewMCPToolName(name string) bool {
	return validReviewToolName(name) && !strings.HasPrefix(name, "_")
}

// CanonicalReviewToolPolicy validates a policy and returns a copy with sorted,
// duplicate-free tool lists and no empty server entries.
func CanonicalReviewToolPolicy(p *ReviewToolPolicy) (*ReviewToolPolicy, error) {
	if p == nil {
		return nil, nil
	}
	if p.Mode != "read-only" {
		return nil, fmt.Errorf("review tools mode must be %q", "read-only")
	}
	result := &ReviewToolPolicy{Mode: p.Mode, MCP: make(map[string][]string, len(p.MCP))}
	for server, tools := range p.MCP {
		if !validReviewMCPServerName(server) {
			return nil, fmt.Errorf("invalid review MCP server name %q", server)
		}
		for _, tool := range tools {
			if !validReviewMCPToolName(tool) {
				return nil, fmt.Errorf("invalid review MCP tool name %q for server %q", tool, server)
			}
		}
		if len(tools) == 0 {
			continue
		}
		sorted := slices.Clone(tools)
		slices.Sort(sorted)
		result.MCP[server] = slices.Compact(sorted)
	}
	return result, nil
}

// ResolveReviewSettings returns the settings of a review type. A member policy
// replaces the type policy as a whole.
func ResolveReviewSettings(reviewType string, memberTools *ReviewToolPolicy, repoCfg *RepoConfig, globalCfg *Config) (ReviewSettings, error) {
	var settings ReviewSettings
	if !IsBuiltInReviewType(reviewType) {
		resolved, ok := ResolveCustomReviewTypeFromConfig(reviewType, repoCfg, globalCfg)
		if !ok {
			return settings, fmt.Errorf("unknown review type %q", reviewType)
		}
		settings.Tools = resolved.Spec.Tools
		settings.BinaryEvidence = resolved.Spec.BinaryEvidence
	}
	if memberTools != nil {
		settings.Tools = memberTools
	}
	var err error
	settings.Tools, err = CanonicalReviewToolPolicy(settings.Tools)
	return settings, err
}

func validateReviewMCPAllowedTools(entries []string) error {
	for _, entry := range entries {
		server, tool, ok := strings.Cut(entry, "/")
		if !ok || !validReviewMCPServerName(server) || !validReviewMCPToolName(tool) {
			return fmt.Errorf("review_mcp_allowed_tools entry %q must be an exact server/tool pair", entry)
		}
	}
	return nil
}
