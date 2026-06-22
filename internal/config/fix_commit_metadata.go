package config

import (
	"fmt"
	"net/mail"
	"strings"
)

const (
	fixCommitAuthorKey      = "fix_commit_author"
	fixCommitCoAuthorsKey   = "fix_commit_co_authored_by"
	fixCommitIdentityFormat = "Name <email>"
)

// FixCommitMetadata holds optional metadata for commits produced by fix-like
// workflows. Roborev-owned commit paths apply it directly; foreground
// agent-owned paths receive it as prompt instructions.
type FixCommitMetadata struct {
	Author    string
	CoAuthors []string
}

// Empty reports whether no fix commit metadata is configured.
func (m FixCommitMetadata) Empty() bool {
	return m.Author == "" && len(m.CoAuthors) == 0
}

// ResolveFixCommitMetadata resolves fix commit metadata from repo and global
// config. Repo values override global values independently per field. Explicit
// empty repo values clear global defaults when raw TOML key presence can prove
// they were set.
func ResolveFixCommitMetadata(repoPath string, globalCfg *Config) (FixCommitMetadata, error) {
	repoCfg, err := LoadRepoConfig(repoPath)
	if err != nil {
		return FixCommitMetadata{}, err
	}
	rawRepo, err := LoadRawRepo(repoPath)
	if err != nil {
		return FixCommitMetadata{}, err
	}
	rawGlobal, err := LoadRawGlobal()
	if err != nil {
		return FixCommitMetadata{}, err
	}
	return ResolveFixCommitMetadataFrom(repoCfg, globalCfg, rawRepo, rawGlobal)
}

// ResolveFixCommitMetadataFrom is the config-taking core of
// ResolveFixCommitMetadata. rawRepo/rawGlobal should be the decoded TOML maps
// for the matching configs so explicit empty values can clear inherited values.
func ResolveFixCommitMetadataFrom(
	repoCfg *RepoConfig,
	globalCfg *Config,
	rawRepo, rawGlobal map[string]any,
) (FixCommitMetadata, error) {
	author, err := resolveFixCommitAuthor(repoCfg, globalCfg, rawRepo, rawGlobal)
	if err != nil {
		return FixCommitMetadata{}, err
	}
	coAuthors, err := resolveFixCommitCoAuthors(repoCfg, globalCfg, rawRepo, rawGlobal)
	if err != nil {
		return FixCommitMetadata{}, err
	}
	return FixCommitMetadata{Author: author, CoAuthors: coAuthors}, nil
}

func resolveFixCommitAuthor(
	repoCfg *RepoConfig,
	globalCfg *Config,
	rawRepo, rawGlobal map[string]any,
) (string, error) {
	value := ""
	present := false
	if repoCfg != nil && rawKeyPresent(rawRepo, fixCommitAuthorKey) {
		value = repoCfg.FixCommitAuthor
		present = true
	} else if repoCfg != nil && strings.TrimSpace(repoCfg.FixCommitAuthor) != "" {
		value = repoCfg.FixCommitAuthor
		present = true
	} else if globalCfg != nil && rawKeyPresent(rawGlobal, fixCommitAuthorKey) {
		value = globalCfg.FixCommitAuthor
		present = true
	} else if globalCfg != nil && strings.TrimSpace(globalCfg.FixCommitAuthor) != "" {
		value = globalCfg.FixCommitAuthor
		present = true
	}
	if !present || strings.TrimSpace(value) == "" {
		return "", nil
	}
	identity, err := ValidateFixCommitIdentity(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", fixCommitAuthorKey, err)
	}
	return identity, nil
}

func resolveFixCommitCoAuthors(
	repoCfg *RepoConfig,
	globalCfg *Config,
	rawRepo, rawGlobal map[string]any,
) ([]string, error) {
	var values []string
	present := false
	if repoCfg != nil && rawKeyPresent(rawRepo, fixCommitCoAuthorsKey) {
		values = repoCfg.FixCommitCoAuthoredBy
		present = true
	} else if repoCfg != nil && len(repoCfg.FixCommitCoAuthoredBy) > 0 {
		values = repoCfg.FixCommitCoAuthoredBy
		present = true
	} else if globalCfg != nil && rawKeyPresent(rawGlobal, fixCommitCoAuthorsKey) {
		values = globalCfg.FixCommitCoAuthoredBy
		present = true
	} else if globalCfg != nil && len(globalCfg.FixCommitCoAuthoredBy) > 0 {
		values = globalCfg.FixCommitCoAuthoredBy
		present = true
	}
	if !present {
		return nil, nil
	}
	if len(values) == 0 {
		return nil, nil
	}
	coAuthors := make([]string, 0, len(values))
	for i, value := range values {
		identity, err := ValidateFixCommitIdentity(value)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", fixCommitCoAuthorsKey, i, err)
		}
		coAuthors = append(coAuthors, identity)
	}
	return coAuthors, nil
}

// ValidateFixCommitIdentity validates a Git author/trailer identity in
// conventional "Name <email>" form and returns the trimmed original spelling.
func ValidateFixCommitIdentity(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("empty identity; expected %s", fixCommitIdentityFormat)
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid identity %q; expected %s", trimmed, fixCommitIdentityFormat)
	}
	if addr.Name == "" || addr.Address == "" ||
		!strings.Contains(trimmed, "<") || !strings.Contains(trimmed, ">") {
		return "", fmt.Errorf("invalid identity %q; expected %s", trimmed, fixCommitIdentityFormat)
	}
	return trimmed, nil
}

func rawKeyPresent(raw map[string]any, key string) bool {
	return raw != nil && IsKeyInTOMLFile(raw, key)
}
