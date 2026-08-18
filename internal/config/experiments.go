package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strings"

	tomlv2 "github.com/pelletier/go-toml/v2"
)

// ExperimentWorkflow identifies a daemon-backed review entry point that may
// participate in a configuration experiment.
type ExperimentWorkflow string

const (
	ExperimentWorkflowReview ExperimentWorkflow = "review"
	ExperimentWorkflowCI     ExperimentWorkflow = "ci"
)

// ExperimentArm is the resolved branch assignment for an experiment.
type ExperimentArm string

const (
	ExperimentArmDefault      ExperimentArm = "default"
	ExperimentArmExperimental ExperimentArm = "experiment"
)

// ExperimentDefinition is a named configuration experiment. Pointer fields
// preserve the distinction between an omitted value and an explicit zero.
type ExperimentDefinition struct {
	Enabled   *bool                `toml:"enabled,omitempty" json:"-"`
	Ratio     *float64             `toml:"ratio,omitempty" json:"ratio"`
	Workflows []ExperimentWorkflow `toml:"workflows,omitempty" json:"workflows"`
	Config    map[string]any       `toml:"config,omitempty" json:"config"`
}

// ExperimentSubject is the stable repository-scoped branch identity used for
// assignment. SourceRepo is required for CI pull requests and empty for local
// reviews.
type ExperimentSubject struct {
	Repository string
	SourceRepo string
	Branch     string
}

// ExperimentSelectionInput contains the already-loaded configuration and the
// branch identity for one review unit.
type ExperimentSelectionInput struct {
	Workflow ExperimentWorkflow
	Subject  ExperimentSubject
	Global   *Config
	Repo     *RepoConfig
	RawRepo  map[string]any
}

// ExperimentAssignment is immutable attribution stored with a review unit.
type ExperimentAssignment struct {
	ID                  string        `json:"id"`
	Arm                 ExperimentArm `json:"arm"`
	SubjectHash         string        `json:"subject_hash"`
	DefinitionHash      string        `json:"definition_hash"`
	DefinitionJSON      string        `json:"-"`
	EffectiveConfigHash string        `json:"effective_config_hash"`
}

// ExperimentSelection contains the repository configuration after applying an
// experimental overlay and the assignment that caused it. Assignment is nil
// when the review is not enrolled.
type ExperimentSelection struct {
	RepoConfig    *RepoConfig
	RawRepoConfig map[string]any
	SubjectHash   string
	Assignment    *ExperimentAssignment
}

type canonicalExperimentDefinition struct {
	Ratio     float64              `json:"ratio"`
	Workflows []ExperimentWorkflow `json:"workflows"`
	Config    map[string]any       `json:"config"`
}

var reviewExperimentOverlayKeys = map[string]struct{}{
	"agent":                         {},
	"model":                         {},
	"backup_agent":                  {},
	"backup_model":                  {},
	"review_reasoning":              {},
	"review_min_severity":           {},
	"reuse_review_session":          {},
	"reuse_review_session_lookback": {},
	"review_agent":                  {},
	"review_agent_fast":             {},
	"review_agent_low":              {},
	"review_agent_standard":         {},
	"review_agent_medium":           {},
	"review_agent_thorough":         {},
	"review_agent_high":             {},
	"review_agent_xhigh":            {},
	"review_agent_maximum":          {},
	"review_agent_max":              {},
	"review_model":                  {},
	"review_model_fast":             {},
	"review_model_low":              {},
	"review_model_standard":         {},
	"review_model_medium":           {},
	"review_model_thorough":         {},
	"review_model_high":             {},
	"review_model_xhigh":            {},
	"review_model_maximum":          {},
	"review_model_max":              {},
	"review_backup_agent":           {},
	"review_backup_model":           {},
	"security_agent":                {},
	"security_agent_fast":           {},
	"security_agent_low":            {},
	"security_agent_standard":       {},
	"security_agent_medium":         {},
	"security_agent_thorough":       {},
	"security_agent_high":           {},
	"security_agent_xhigh":          {},
	"security_agent_maximum":        {},
	"security_agent_max":            {},
	"security_model":                {},
	"security_model_fast":           {},
	"security_model_low":            {},
	"security_model_standard":       {},
	"security_model_medium":         {},
	"security_model_thorough":       {},
	"security_model_high":           {},
	"security_model_xhigh":          {},
	"security_model_maximum":        {},
	"security_model_max":            {},
	"security_backup_agent":         {},
	"security_backup_model":         {},
	"design_agent":                  {},
	"design_agent_fast":             {},
	"design_agent_low":              {},
	"design_agent_standard":         {},
	"design_agent_medium":           {},
	"design_agent_thorough":         {},
	"design_agent_high":             {},
	"design_agent_xhigh":            {},
	"design_agent_maximum":          {},
	"design_agent_max":              {},
	"design_model":                  {},
	"design_model_fast":             {},
	"design_model_low":              {},
	"design_model_standard":         {},
	"design_model_medium":           {},
	"design_model_thorough":         {},
	"design_model_high":             {},
	"design_model_xhigh":            {},
	"design_model_maximum":          {},
	"design_model_max":              {},
	"design_backup_agent":           {},
	"design_backup_model":           {},
	"review":                        {},
	"ci":                            {},
}

var reviewExperimentCIKeys = map[string]struct{}{
	"agents":       {},
	"review_types": {},
	"reviews":      {},
	"panel":        {},
	"reasoning":    {},
	"min_severity": {},
}

// SelectReviewExperiment validates the effective experiment configuration,
// assigns the branch deterministically, and applies the experimental overlay.
func SelectReviewExperiment(in ExperimentSelectionInput) (ExperimentSelection, error) {
	result := ExperimentSelection{RepoConfig: in.Repo}
	if !validExperimentSubject(in.Workflow, in.Subject) {
		return result, nil
	}
	result.SubjectHash = hashExperimentSubject(in.Subject)

	definitions, err := mergeExperimentDefinitions(in.Global, in.Repo)
	if err != nil {
		return ExperimentSelection{}, err
	}
	var selectedID string
	var selected ExperimentDefinition
	for _, id := range sortedMapKeys(definitions) {
		definition := definitions[id]
		if experimentEnabled(definition) && slices.Contains(definition.Workflows, in.Workflow) {
			if selectedID != "" {
				return ExperimentSelection{}, fmt.Errorf(
					"enabled experiments %q and %q both apply to workflow %q",
					selectedID, id, in.Workflow,
				)
			}
			selectedID, selected = id, definition
		}
	}
	if selectedID == "" {
		return result, nil
	}

	subjectHash := result.SubjectHash
	definitionJSON, definitionHash, err := canonicalizeExperimentDefinition(selected)
	if err != nil {
		return ExperimentSelection{}, fmt.Errorf("experiment %q: %w", selectedID, err)
	}
	arm := assignExperimentArm(selectedID, subjectHash, *selected.Ratio)

	if in.Repo != nil && in.RawRepo == nil {
		return ExperimentSelection{}, fmt.Errorf(
			"experiment %q: repository config is missing its paired raw representation",
			selectedID,
		)
	}
	effectiveRaw, err := effectiveRepoConfigMap(in.Global, in.RawRepo, nil)
	if err != nil {
		return ExperimentSelection{}, err
	}
	if arm == ExperimentArmExperimental {
		effectiveRaw = mergeExperimentMaps(effectiveRaw, selected.Config)
	}
	effectiveCfg, err := decodeExperimentRepoConfig(effectiveRaw)
	if err != nil {
		return ExperimentSelection{}, fmt.Errorf("experiment %q config: %w", selectedID, err)
	}
	effectiveJSON, err := json.Marshal(effectiveRaw)
	if err != nil {
		return ExperimentSelection{}, fmt.Errorf("experiment %q effective config: %w", selectedID, err)
	}
	effectiveHash := sha256Hex(effectiveJSON)

	result.RepoConfig = effectiveCfg
	result.RawRepoConfig = effectiveRaw
	result.Assignment = &ExperimentAssignment{
		ID:                  selectedID,
		Arm:                 arm,
		SubjectHash:         subjectHash,
		DefinitionHash:      definitionHash,
		DefinitionJSON:      string(definitionJSON),
		EffectiveConfigHash: effectiveHash,
	}
	return result, nil
}

func mergeExperimentDefinitions(global *Config, repo *RepoConfig) (map[string]ExperimentDefinition, error) {
	merged := make(map[string]ExperimentDefinition)
	if global != nil {
		if err := validateExperimentEntries(global.Experiments, true); err != nil {
			return nil, err
		}
		maps.Copy(merged, global.Experiments)
	}
	if repo != nil {
		for id, definition := range repo.Experiments {
			if _, exists := merged[id]; exists && !experimentEnabledOnly(definition) {
				return nil, fmt.Errorf("repository experiment %q may override only enabled; use a new experiment ID for definition changes", id)
			}
		}
		if err := validateExperimentEntries(repo.Experiments, false); err != nil {
			return nil, err
		}
		for id, definition := range repo.Experiments {
			base, exists := merged[id]
			if !exists {
				if !experimentDefinitionComplete(definition) {
					return nil, fmt.Errorf("repository experiment %q overrides no global definition", id)
				}
				merged[id] = definition
				continue
			}
			base.Enabled = definition.Enabled
			merged[id] = base
		}
	}
	return merged, nil
}

func validateExperimentEntries(entries map[string]ExperimentDefinition, global bool) error {
	enabledWorkflows := make(map[ExperimentWorkflow]string)
	for _, id := range sortedMapKeys(entries) {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("experiment ID must not be empty")
		}
		definition := entries[id]
		if !global && experimentEnabledOnly(definition) {
			continue
		}
		if !experimentDefinitionComplete(definition) {
			return fmt.Errorf("experiment %q requires ratio, workflows, and config", id)
		}
		if err := validateExperimentDefinition(id, definition); err != nil {
			return err
		}
		if experimentEnabled(definition) {
			for _, workflow := range definition.Workflows {
				if prior := enabledWorkflows[workflow]; prior != "" {
					return fmt.Errorf("enabled experiments %q and %q both apply to workflow %q", prior, id, workflow)
				}
				enabledWorkflows[workflow] = id
			}
		}
	}
	return nil
}

func validateExperimentDefinition(id string, definition ExperimentDefinition) error {
	ratio := *definition.Ratio
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
		return fmt.Errorf("experiment %q ratio must be between 0 and 1", id)
	}
	seen := make(map[ExperimentWorkflow]struct{}, len(definition.Workflows))
	for _, workflow := range definition.Workflows {
		switch workflow {
		case ExperimentWorkflowReview, ExperimentWorkflowCI:
		default:
			return fmt.Errorf("experiment %q has unknown workflow %q", id, workflow)
		}
		if _, duplicate := seen[workflow]; duplicate {
			return fmt.Errorf("experiment %q repeats workflow %q", id, workflow)
		}
		seen[workflow] = struct{}{}
	}
	if err := validateExperimentOverlay(definition.Config); err != nil {
		return fmt.Errorf("experiment %q config: %w", id, err)
	}
	return nil
}

func validateExperimentOverlay(overlay map[string]any) error {
	for key, value := range overlay {
		if key == "experiments" {
			return fmt.Errorf("experiments cannot contain nested experiments")
		}
		if _, allowed := reviewExperimentOverlayKeys[key]; !allowed {
			return fmt.Errorf("key %q is not a review-time configuration setting", key)
		}
		if key != "ci" {
			continue
		}
		ci, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("ci must be a table")
		}
		for ciKey := range ci {
			if _, allowed := reviewExperimentCIKeys[ciKey]; !allowed {
				return fmt.Errorf("key %q is not an experimental CI review setting", "ci."+ciKey)
			}
		}
	}
	return nil
}

func canonicalizeExperimentDefinition(definition ExperimentDefinition) ([]byte, string, error) {
	workflows := slices.Clone(definition.Workflows)
	slices.Sort(workflows)
	encoded, err := json.Marshal(canonicalExperimentDefinition{
		Ratio: *definition.Ratio, Workflows: workflows, Config: definition.Config,
	})
	if err != nil {
		return nil, "", err
	}
	return encoded, sha256Hex(encoded), nil
}

func validExperimentSubject(workflow ExperimentWorkflow, subject ExperimentSubject) bool {
	if strings.TrimSpace(subject.Repository) == "" || strings.TrimSpace(subject.Branch) == "" {
		return false
	}
	return workflow != ExperimentWorkflowCI || strings.TrimSpace(subject.SourceRepo) != ""
}

func hashExperimentSubject(subject ExperimentSubject) string {
	var encoded bytes.Buffer
	encoded.WriteString("roborev-review-subject-v1")
	for _, field := range []string{subject.Repository, subject.SourceRepo, subject.Branch} {
		_ = binary.Write(&encoded, binary.BigEndian, uint32(len(field)))
		encoded.WriteString(field)
	}
	return sha256Hex(encoded.Bytes())
}

func assignExperimentArm(id, subjectHash string, ratio float64) ExperimentArm {
	if ratio <= 0 {
		return ExperimentArmDefault
	}
	if ratio >= 1 {
		return ExperimentArmExperimental
	}
	h := sha256.New()
	h.Write([]byte("roborev-review-experiment-v1"))
	h.Write([]byte(id))
	h.Write([]byte(subjectHash))
	sum := h.Sum(nil)
	value := binary.BigEndian.Uint64(sum[:8])

	thresholdFloat := new(big.Float).SetPrec(256).SetFloat64(ratio)
	thresholdFloat.Mul(thresholdFloat, new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 64)))
	threshold, _ := thresholdFloat.Uint64()
	if value < threshold {
		return ExperimentArmExperimental
	}
	return ExperimentArmDefault
}

func effectiveRepoConfigMap(global *Config, rawRepo, overlay map[string]any) (map[string]any, error) {
	base, err := globalReviewConfigMap(global)
	if err != nil {
		return nil, err
	}
	if rawRepo != nil {
		base = mergeExperimentMaps(base, rawRepo)
	}
	if base == nil {
		base = make(map[string]any)
	}
	delete(base, "experiments")
	return mergeExperimentMaps(base, overlay), nil
}

func globalReviewConfigMap(global *Config) (map[string]any, error) {
	if global == nil {
		return make(map[string]any), nil
	}
	data, err := tomlv2.Marshal(global)
	if err != nil {
		return nil, fmt.Errorf("encode global config: %w", err)
	}
	var rawGlobal map[string]any
	if err := tomlv2.Unmarshal(data, &rawGlobal); err != nil {
		return nil, fmt.Errorf("decode global config map: %w", err)
	}
	result := make(map[string]any)
	for key, value := range rawGlobal {
		if key == "agent" || key == "ci" {
			continue
		}
		if _, allowed := reviewExperimentOverlayKeys[key]; allowed {
			result[key] = cloneExperimentValue(value)
		}
	}
	copyGlobalDefault := func(globalKey, repoKey string) {
		if value, ok := rawGlobal[globalKey]; ok {
			result[repoKey] = cloneExperimentValue(value)
		}
	}
	copyGlobalDefault("default_agent", "agent")
	copyGlobalDefault("default_model", "model")
	copyGlobalDefault("default_backup_agent", "backup_agent")
	copyGlobalDefault("default_backup_model", "backup_model")
	copyGlobalDefault("default_max_prompt_size", "max_prompt_size")
	if rawCI, ok := rawGlobal["ci"].(map[string]any); ok {
		ci := make(map[string]any)
		for key, value := range rawCI {
			if _, allowed := reviewExperimentCIKeys[key]; allowed {
				ci[key] = cloneExperimentValue(value)
			}
		}
		if len(ci) > 0 {
			result["ci"] = ci
		}
	}
	return result, nil
}

func decodeExperimentRepoConfig(raw map[string]any) (*RepoConfig, error) {
	data, err := tomlv2.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var cfg RepoConfig
	decoder := tomlv2.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := validateConfig(&cfg, cfg.ACP); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// FingerprintExperimentConfig returns the canonical hash stored with an
// assignment after the caller has resolved the complete review-unit plan.
func FingerprintExperimentConfig(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(encoded), nil
}

func mergeExperimentMaps(base, overlay map[string]any) map[string]any {
	merged := cloneExperimentMap(base)
	if merged == nil {
		merged = make(map[string]any)
	}
	for key, value := range overlay {
		if overlayMap, ok := value.(map[string]any); ok {
			if baseMap, ok := merged[key].(map[string]any); ok {
				merged[key] = mergeExperimentMaps(baseMap, overlayMap)
				continue
			}
		}
		merged[key] = cloneExperimentValue(value)
	}
	return merged
}

func cloneExperimentMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneExperimentValue(value)
	}
	return cloned
}

func cloneExperimentValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneExperimentMap(typed)
	case []map[string]any:
		cloned := make([]map[string]any, len(typed))
		for i := range typed {
			cloned[i] = cloneExperimentMap(typed[i])
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i := range typed {
			cloned[i] = cloneExperimentValue(typed[i])
		}
		return cloned
	default:
		return value
	}
}

func experimentDefinitionComplete(definition ExperimentDefinition) bool {
	return definition.Ratio != nil && len(definition.Workflows) > 0 && len(definition.Config) > 0
}

func experimentEnabledOnly(definition ExperimentDefinition) bool {
	return definition.Enabled != nil && definition.Ratio == nil && len(definition.Workflows) == 0 && len(definition.Config) == 0
}

func experimentEnabled(definition ExperimentDefinition) bool {
	return definition.Enabled != nil && *definition.Enabled
}

func mapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := mapKeys(values)
	slices.Sort(keys)
	return keys
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
