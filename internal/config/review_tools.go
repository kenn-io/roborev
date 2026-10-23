package config

import (
	"bytes"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	tomlv2 "github.com/pelletier/go-toml/v2"

	"go.kenn.io/roborev/internal/git"
)

// ReviewToolPolicy restricts a review's native tools and names its exact MCP
// grants. A nil policy retains ordinary adapter behavior; an empty MCP map
// with mode read-only is an explicit strict policy without MCP access.
type ReviewToolPolicy struct {
	Mode string              `toml:"mode" json:"mode"`
	MCP  map[string][]string `toml:"mcp" json:"mcp"`
}

// ReviewSettings holds effective per-review settings that can be frozen in
// existing member JSON. Operator MCP paths and ceilings never belong here.
type ReviewSettings struct {
	Tools            *ReviewToolPolicy `toml:"tools" json:"tools,omitzero"`
	BinaryEvidence   bool              `toml:"binary_evidence" json:"binary_evidence"`
	EvidenceManifest string            `toml:"evidence_manifest" json:"evidence_manifest"`
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

func validReviewToolName(name string) bool {
	return reviewToolNamePattern.MatchString(name) && !strings.Contains(name, "__")
}

// CanonicalReviewToolPolicy validates exact names and returns a deep copy with
// sorted, duplicate-free tool lists and no empty server entries. It never turns
// absence into strict mode.
func CanonicalReviewToolPolicy(p *ReviewToolPolicy) (*ReviewToolPolicy, error) {
	if p == nil {
		return nil, nil
	}
	if p.Mode != "read-only" {
		return nil, fmt.Errorf("review tools mode must be %q", "read-only")
	}
	result := &ReviewToolPolicy{Mode: p.Mode, MCP: make(map[string][]string, len(p.MCP))}
	for _, server := range slices.Sorted(maps.Keys(p.MCP)) {
		if !validReviewToolName(server) {
			return nil, fmt.Errorf("invalid review MCP server name %q", server)
		}
		tools := slices.Clone(p.MCP[server])
		for _, tool := range tools {
			if !validReviewToolName(tool) {
				return nil, fmt.Errorf("invalid review MCP tool name %q for server %q", tool, server)
			}
		}
		if len(tools) == 0 {
			continue
		}
		slices.Sort(tools)
		result.MCP[server] = slices.Compact(tools)
	}
	return result, nil
}

// ResolveReviewSettings applies whole-type replacement followed by an optional
// member policy replacement. Evidence remains owned by the custom review type.
func ResolveReviewSettings(reviewType string, memberTools *ReviewToolPolicy, repoCfg *RepoConfig, globalCfg *Config) (ReviewSettings, error) {
	var settings ReviewSettings
	if !IsBuiltInReviewType(reviewType) {
		resolved, ok := ResolveCustomReviewTypeFromConfig(reviewType, repoCfg, globalCfg)
		if !ok {
			return settings, fmt.Errorf("unknown review type %q", reviewType)
		}
		settings.Tools = resolved.Spec.Tools
		settings.BinaryEvidence = resolved.Spec.BinaryEvidence
		settings.EvidenceManifest = resolved.Spec.EvidenceManifest
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
		if !ok || !validReviewToolName(server) || !validReviewToolName(tool) {
			return fmt.Errorf("review_mcp_allowed_tools entry %q must be an exact server/tool pair", entry)
		}
	}
	return nil
}

// LoadTrustedRepoConfig never falls back to working-tree configuration. A
// missing file is authoritative absence, while an unknown ref is an error.
func LoadTrustedRepoConfig(repoPath, ref string) (RepoConfigSource, error) {
	ref = strings.TrimSpace(ref)
	source := RepoConfigSource{Ref: ref}
	if ref == "" {
		return source, fmt.Errorf("trusted repository config requires a nonempty ref")
	}
	if _, err := git.ResolveSHA(repoPath, ref+"^{tree}"); err != nil {
		return source, fmt.Errorf("resolve trusted config ref: %w", err)
	}
	cfg, raw, err := LoadRepoConfigFromRefWithRaw(repoPath, ref)
	if err != nil {
		return source, err
	}
	source.Config, source.Raw = cfg, raw
	return source, nil
}

// An experiment's present tools table is independently complete and atomic;
// merging first would let a partial policy borrow authority from its base.
func validateExperimentReviewTools(value any) error {
	review, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("experiment review must be a table")
	}
	panels, _ := review["panels"].(map[string]any)
	for name, entry := range panels {
		fields, _ := entry.(map[string]any)
		if _, present := fields["tools"]; present {
			return fmt.Errorf("experiment review.panels.%s.tools is not supported; set tools on a subagent", name)
		}
	}
	for _, section := range []string{"types", "subagents"} {
		entries, _ := review[section].(map[string]any)
		for name, entry := range entries {
			fields, _ := entry.(map[string]any)
			tools, present := fields["tools"]
			if !present {
				continue
			}
			table, ok := tools.(map[string]any)
			if !ok {
				return fmt.Errorf("experiment review.%s.%s.tools must be a table", section, name)
			}
			data, err := tomlv2.Marshal(table)
			if err != nil {
				return fmt.Errorf("encode experiment review tools: %w", err)
			}
			var policy ReviewToolPolicy
			if err := tomlv2.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&policy); err != nil {
				return fmt.Errorf("decode experiment review tools: %w", err)
			}
			if _, err := CanonicalReviewToolPolicy(&policy); err != nil {
				return fmt.Errorf("experiment review.%s.%s.tools: %w", section, name, err)
			}
		}
	}
	return nil
}
