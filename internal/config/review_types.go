package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const customReviewTypeNameMaxLength = 64

var (
	customReviewTypeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	customIncludeNamePattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
)

// ReviewTypeSpec defines a schema-constrained custom review prompt. Template
// and include paths are resolved when the prompt is built so one global type
// may refer to files in each repository by relative path.
type ReviewTypeSpec struct {
	Template  string            `toml:"template" comment:"Go template containing the custom review rubric."`
	Includes  map[string]string `toml:"includes" comment:"Named local files exposed to the template through .Includes."`
	Agent     string            `toml:"agent" comment:"Structured-output agent override for this review type."`
	Model     string            `toml:"model" comment:"Model override for this review type."`
	Reasoning string            `toml:"reasoning" comment:"Reasoning level for this review type."`
}

func (s ReviewTypeSpec) Clone() ReviewTypeSpec {
	cloned := s
	cloned.Includes = maps.Clone(s.Includes)
	return cloned
}

func (s ReviewTypeSpec) validate(name string) error {
	var errs []error
	if strings.TrimSpace(s.Template) == "" {
		errs = append(errs, fmt.Errorf("review type %q has no template", name))
	}
	if reasoning := strings.TrimSpace(s.Reasoning); reasoning != "" {
		if _, err := NormalizeReasoning(reasoning); err != nil {
			errs = append(errs, fmt.Errorf("review type %q: %w", name, err))
		}
	}
	for includeName, path := range s.Includes {
		if !customIncludeNamePattern.MatchString(includeName) {
			errs = append(errs, fmt.Errorf(
				"review type %q include %q has an invalid name", name, includeName,
			))
		}
		if strings.TrimSpace(path) == "" {
			errs = append(errs, fmt.Errorf(
				"review type %q include %q has no path", name, includeName,
			))
		}
	}
	return errors.Join(errs...)
}

func validateCustomReviewTypes(types map[string]ReviewTypeSpec) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(types)) {
		switch {
		case len(name) > customReviewTypeNameMaxLength:
			errs = append(errs, fmt.Errorf(
				"review type name %q exceeds %d characters",
				name, customReviewTypeNameMaxLength,
			))
		case !customReviewTypeNamePattern.MatchString(name):
			errs = append(errs, fmt.Errorf(
				"review type name %q must match %s",
				name, customReviewTypeNamePattern,
			))
		case isReservedCustomReviewTypeName(name):
			errs = append(errs, fmt.Errorf(
				"review type name %q is reserved", name,
			))
		default:
			errs = append(errs, types[name].validate(name))
		}
	}
	return errors.Join(errs...)
}

func isReservedCustomReviewTypeName(name string) bool {
	return IsBuiltInReviewType(name) || slices.Contains(
		[]string{"general", "review", "fix", "refine", "classify"}, name,
	)
}

// ResolvedReviewType identifies the effective custom definition. A repository
// definition replaces the complete same-name global definition.
type ResolvedReviewType struct {
	Name string
	Spec ReviewTypeSpec
}

func ResolveCustomReviewTypeFromConfig(
	name string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) (ResolvedReviewType, bool) {
	name = strings.TrimSpace(name)
	if repoCfg != nil {
		if spec, ok := repoCfg.Review.Types[name]; ok {
			return ResolvedReviewType{Name: name, Spec: spec.Clone()}, true
		}
	}
	if globalCfg != nil {
		if spec, ok := globalCfg.Review.Types[name]; ok {
			return ResolvedReviewType{Name: name, Spec: spec.Clone()}, true
		}
	}
	return ResolvedReviewType{}, false
}

func CustomReviewTypeNamesFromConfig(
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	names := make(map[string]struct{})
	if globalCfg != nil {
		for name := range globalCfg.Review.Types {
			names[name] = struct{}{}
		}
	}
	if repoCfg != nil {
		for name := range repoCfg.Review.Types {
			names[name] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(names))
}

func ReviewTypesFromConfig(
	repoCfg *RepoConfig,
	globalCfg *Config,
) []string {
	types := ExplicitReviewTypes()
	return append(types, CustomReviewTypeNamesFromConfig(repoCfg, globalCfg)...)
}

func ValidateReviewTypesFromConfig(
	types []string,
	repoCfg *RepoConfig,
	globalCfg *Config,
) ([]string, error) {
	custom := make(map[string]bool)
	for _, name := range CustomReviewTypeNamesFromConfig(repoCfg, globalCfg) {
		custom[name] = true
	}
	return validateReviewTypes(types, custom)
}

func ResolveReviewReasoningForTypeFromConfig(
	explicit string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	reviewType string,
) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return ResolveReviewReasoningFromConfig(explicit, repoCfg, globalCfg)
	}
	if resolved, ok := ResolveCustomReviewTypeFromConfig(
		reviewType, repoCfg, globalCfg,
	); ok && strings.TrimSpace(resolved.Spec.Reasoning) != "" {
		return NormalizeReasoning(resolved.Spec.Reasoning)
	}
	return ResolveReviewReasoningFromConfig("", repoCfg, globalCfg)
}

func customReviewTypeField(
	types map[string]ReviewTypeSpec,
	name string,
	isAgent bool,
) string {
	spec, ok := types[name]
	if !ok {
		return ""
	}
	if isAgent {
		return strings.TrimSpace(spec.Agent)
	}
	return strings.TrimSpace(spec.Model)
}
