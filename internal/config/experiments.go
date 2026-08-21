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
// assignment.
type ExperimentSubject struct {
	Repository string
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
	ID             string        `json:"id"`
	Arm            ExperimentArm `json:"arm"`
	SubjectHash    string        `json:"subject_hash"`
	DefinitionHash string        `json:"definition_hash"`
	DefinitionJSON string        `json:"-"`
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

// ValidateExperimentConfigs validates every materialized experiment overlay
// after merging global definitions with repository enablement overrides.
func ValidateExperimentConfigs(
	global *Config, repo *RepoConfig, rawRepo map[string]any,
) error {
	if repo != nil && rawRepo == nil {
		return markExperimentConfigError(
			fmt.Errorf("repository config is missing its paired raw representation"),
		)
	}
	definitions, err := mergeExperimentDefinitions(global, repo)
	if err != nil {
		return markExperimentConfigError(err)
	}
	return validateMaterializedExperimentConfigs(global, rawRepo, definitions)
}

// ValidateRepoExperimentConfigs validates complete experiment definitions in
// repository config. Enablement-only entries need their global definition and
// are validated by ValidateExperimentConfigs in merged scope.
func ValidateRepoExperimentConfigs(repo *RepoConfig, rawRepo map[string]any) error {
	if repo == nil {
		return nil
	}
	if rawRepo == nil {
		return markExperimentConfigError(
			fmt.Errorf("repository config is missing its paired raw representation"),
		)
	}
	if err := validateExperimentEntries(repo.Experiments, false); err != nil {
		return markExperimentConfigError(err)
	}
	definitions := make(map[string]ExperimentDefinition)
	for id, definition := range repo.Experiments {
		if experimentDefinitionComplete(definition) {
			definitions[id] = definition
		}
	}
	return validateMaterializedExperimentConfigs(nil, rawRepo, definitions)
}

func validateMaterializedExperimentConfigs(
	global *Config, rawRepo map[string]any,
	definitions map[string]ExperimentDefinition,
) error {
	for _, id := range sortedMapKeys(definitions) {
		effectiveRaw, err := applyExperimentOverlay(
			global, rawRepo, definitions[id].Config,
		)
		if err != nil {
			return markExperimentConfigError(fmt.Errorf("experiment %q config: %w", id, err))
		}
		effectiveCfg, err := decodeExperimentRepoConfig(effectiveRaw)
		if err != nil {
			return markExperimentConfigError(fmt.Errorf("experiment %q config: %w", id, err))
		}
		effectiveCfg.experimentOverlay = cloneExperimentMap(definitions[id].Config)
		if err := ValidateEffectiveReviewConfig(global, effectiveCfg); err != nil {
			return markExperimentConfigError(fmt.Errorf("experiment %q config: %w", id, err))
		}
	}
	return nil
}

// ValidateEffectiveReviewConfig validates review-time settings after merging
// global and repository configuration.
func ValidateEffectiveReviewConfig(global *Config, effective *RepoConfig) error {
	if err := effective.Validate(); err != nil {
		return err
	}
	if _, err := ResolveReviewReasoningFromConfig("", effective, global); err != nil {
		return fmt.Errorf("review_reasoning: %w", err)
	}
	if effective != nil {
		if _, err := NormalizeReasoning(effective.CI.Reasoning); err != nil {
			return fmt.Errorf("ci.reasoning: %w", err)
		}
	}
	if err := MergeReviewConfigFromConfig(effective, global).Validate(); err != nil {
		return fmt.Errorf("review: %w", err)
	}
	if panelName := ResolveCIPanelName(effective, global); panelName != "" {
		if _, _, err := ResolveCIPanel(panelName, effective, global); err != nil {
			return fmt.Errorf("ci.panel: %w", err)
		}
	}
	return nil
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
	if !validExperimentSubject(in.Subject) {
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
	effectiveRaw := cloneExperimentMap(in.RawRepo)
	effectiveCfg := in.Repo
	overlaidRaw, err := applyExperimentOverlay(in.Global, effectiveRaw, selected.Config)
	if err != nil {
		return ExperimentSelection{}, err
	}
	overlaidCfg, err := decodeExperimentRepoConfig(overlaidRaw)
	if err != nil {
		return ExperimentSelection{}, fmt.Errorf("experiment %q config: %w", selectedID, err)
	}
	overlaidCfg.experimentOverlay = cloneExperimentMap(selected.Config)
	if err := ValidateEffectiveReviewConfig(in.Global, overlaidCfg); err != nil {
		return ExperimentSelection{}, fmt.Errorf("experiment %q config: %w", selectedID, err)
	}
	if arm == ExperimentArmExperimental {
		effectiveRaw = overlaidRaw
		effectiveCfg = overlaidCfg
	}
	result.RepoConfig = effectiveCfg
	result.RawRepoConfig = effectiveRaw
	result.Assignment = &ExperimentAssignment{
		ID:             selectedID,
		Arm:            arm,
		SubjectHash:    subjectHash,
		DefinitionHash: definitionHash,
		DefinitionJSON: string(definitionJSON),
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
	if err := validateEnabledExperimentWorkflows(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func validateEnabledExperimentWorkflows(entries map[string]ExperimentDefinition) error {
	enabledWorkflows := make(map[ExperimentWorkflow]string)
	for _, id := range sortedMapKeys(entries) {
		definition := entries[id]
		if !experimentEnabled(definition) {
			continue
		}
		for _, workflow := range definition.Workflows {
			if prior := enabledWorkflows[workflow]; prior != "" {
				return fmt.Errorf(
					"enabled experiments %q and %q both apply to workflow %q",
					prior, id, workflow,
				)
			}
			enabledWorkflows[workflow] = id
		}
	}
	return nil
}

func validateExperimentEntries(entries map[string]ExperimentDefinition, global bool) error {
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
	}
	return validateEnabledExperimentWorkflows(entries)
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

func validExperimentSubject(subject ExperimentSubject) bool {
	return strings.TrimSpace(subject.Repository) != "" && strings.TrimSpace(subject.Branch) != ""
}

func hashExperimentSubject(subject ExperimentSubject) string {
	var encoded bytes.Buffer
	encoded.WriteString("roborev-review-subject-v1")
	for _, field := range []string{subject.Repository, subject.Branch} {
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

// applyExperimentOverlay applies the treatment as a synthetic repository
// layer. Ordinary global and repository settings stay in their original
// layers so the existing typed resolvers retain their precedence. For the
// nested entries whose normal semantics are whole-entry replacement, only an
// entry touched by the overlay is materialized from its effective base and
// recursively merged.
func applyExperimentOverlay(global *Config, rawRepo, overlay map[string]any) (map[string]any, error) {
	effective := mergeExperimentMaps(rawRepo, overlay)
	globalRaw, err := globalExperimentConfigMap(global)
	if err != nil {
		return nil, err
	}

	overlayReview, _ := overlay["review"].(map[string]any)
	repoReview, _ := rawRepo["review"].(map[string]any)
	globalReview, _ := globalRaw["review"].(map[string]any)
	effectiveReview, _ := effective["review"].(map[string]any)
	for _, key := range []string{"subagents", "panels"} {
		overlayEntries, ok := overlayReview[key].(map[string]any)
		if !ok {
			continue
		}
		if effectiveReview == nil {
			effectiveReview = make(map[string]any)
			effective["review"] = effectiveReview
		}
		effectiveEntries, _ := effectiveReview[key].(map[string]any)
		if effectiveEntries == nil {
			effectiveEntries = make(map[string]any)
			effectiveReview[key] = effectiveEntries
		}
		repoEntries, _ := repoReview[key].(map[string]any)
		globalEntries, _ := globalReview[key].(map[string]any)
		for name, entryOverlay := range overlayEntries {
			overlayMap, ok := entryOverlay.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("experiment review.%s.%s must be a table", key, name)
			}
			baseEntry, exists := repoEntries[name]
			if !exists {
				baseEntry = globalEntries[name]
			}
			baseMap, _ := baseEntry.(map[string]any)
			effectiveEntries[name] = mergeExperimentMaps(baseMap, overlayMap)
		}
	}

	// A configured repository CI review matrix replaces the global matrix,
	// including when it is explicitly empty. The treatment may still merge a
	// partial matrix onto that effective base because nested experiment tables
	// are recursive overlays.
	overlayCI, _ := overlay["ci"].(map[string]any)
	if overlayReviews, ok := overlayCI["reviews"].(map[string]any); ok {
		repoCI, _ := rawRepo["ci"].(map[string]any)
		globalCI, _ := globalRaw["ci"].(map[string]any)
		baseReviews, exists := repoCI["reviews"].(map[string]any)
		if !exists {
			baseReviews, _ = globalCI["reviews"].(map[string]any)
		}
		effectiveCI, _ := effective["ci"].(map[string]any)
		if effectiveCI == nil {
			effectiveCI = make(map[string]any)
			effective["ci"] = effectiveCI
		}
		effectiveCI["reviews"] = mergeExperimentMaps(baseReviews, overlayReviews)
	}

	delete(effective, "experiments")
	return effective, nil
}

func globalExperimentConfigMap(global *Config) (map[string]any, error) {
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
	return rawGlobal, nil
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
	if rawCI, ok := raw["ci"].(map[string]any); ok {
		if _, configured := rawCI["reviews"]; configured && cfg.CI.Reviews == nil {
			cfg.CI.Reviews = make(map[string][]string)
		}
	}
	return &cfg, nil
}

func experimentOverlayValue(repoCfg *RepoConfig, path ...string) (any, bool) {
	if repoCfg == nil || repoCfg.experimentOverlay == nil {
		return nil, false
	}
	var current any = repoCfg.experimentOverlay
	for _, key := range path {
		values, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = values[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func experimentOverlayString(repoCfg *RepoConfig, path ...string) (string, bool) {
	value, ok := experimentOverlayValue(repoCfg, path...)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return strings.TrimSpace(text), ok
}

func experimentWorkflowValue(
	repoCfg *RepoConfig, workflow, level string, isAgent bool,
) (string, bool) {
	levels := []string{level}
	if legacy := legacyReasoningFallback(level); legacy != "" {
		levels = append(levels, legacy)
	}
	for _, candidate := range levels {
		if value, ok := experimentOverlayString(
			repoCfg, workflowFieldKey(workflow, candidate, isAgent),
		); ok {
			return value, true
		}
	}
	if value, ok := experimentOverlayString(
		repoCfg, workflowFieldKey(workflow, "", isAgent),
	); ok {
		return value, true
	}
	key := "model"
	if isAgent {
		key = "agent"
	}
	return experimentOverlayString(repoCfg, key)
}

func experimentWorkflowBackupValue(
	repoCfg *RepoConfig, workflow string, isAgent bool,
) (string, bool) {
	kind := "model"
	if isAgent {
		kind = "agent"
	}
	if value, ok := experimentOverlayString(
		repoCfg, workflow+"_backup_"+kind,
	); ok {
		return value, true
	}
	return experimentOverlayString(repoCfg, "backup_"+kind)
}

// ExperimentOverridesWorkflowModel reports whether the selected treatment
// supplies a model for this workflow. Callers use it to distinguish a global
// CI model from a true explicit request, which remains higher precedence.
func ExperimentOverridesWorkflowModel(
	repoCfg *RepoConfig, workflow, level string,
) bool {
	_, ok := experimentWorkflowValue(repoCfg, workflow, level, false)
	return ok
}

// ExperimentOverridesCIReviews reports whether the selected treatment supplies
// a ci.reviews map.
func ExperimentOverridesCIReviews(repoCfg *RepoConfig) bool {
	_, ok := experimentOverlayValue(repoCfg, "ci", "reviews")
	return ok
}

// ExperimentOverridesCIFlatMatrix reports whether the selected treatment
// supplies ci.agents or ci.review_types.
func ExperimentOverridesCIFlatMatrix(repoCfg *RepoConfig) bool {
	if _, ok := experimentOverlayValue(repoCfg, "ci", "agents"); ok {
		return true
	}
	_, ok := experimentOverlayValue(repoCfg, "ci", "review_types")
	return ok
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

// EncodeExperimentConfig returns the canonical JSON and hash for a frozen
// review plan. The JSON lets a later rerun restore the attributed plan after
// execution-time failover mutates the job row.
func EncodeExperimentConfig(value any) (string, string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	return string(encoded), sha256Hex(encoded), nil
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
