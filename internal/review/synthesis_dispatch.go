package review

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	promptpkg "go.kenn.io/roborev/internal/prompt"
)

// SynthesisCheckout is the checkout used for agent execution and prompt file
// handoff. ConfigRepoPath overrides the hook's config path when set; Cleanup
// may be nil.
type SynthesisCheckout struct {
	RepoPath       string
	ConfigRepoPath string
	GitRef         string
	Cleanup        func()
}

// SynthesisCheckoutError wraps a failure to prepare the checkout for the
// review invocation so callers can retry it as infrastructure rather than
// as an agent error.
type SynthesisCheckoutError struct {
	Err error
}

func (e *SynthesisCheckoutError) Error() string { return "prepare checkout: " + e.Err.Error() }
func (e *SynthesisCheckoutError) Unwrap() error { return e.Err }

// SynthesisHooks lets a caller observe the dispatch without duplicating it.
type SynthesisHooks struct {
	// ConfigRepoPath is the fallback for checkouts that do not provide their
	// own configuration path.
	ConfigRepoPath   string
	GlobalConfig     *config.Config
	IgnoreRepoConfig bool
	Tools            *config.ReviewToolPolicy
	// BeforeInvoke runs once, immediately before the agent is called. The
	// daemon uses it to record that an agent actually ran, so it must fire
	// after any checkout preparation that could still fail. An error prevents
	// the invocation.
	BeforeInvoke func(context.Context) error
	// Checkout resolves where read-capable agents run, including when
	// oversized synthesis inputs need repo-local files.
	Checkout func() (SynthesisCheckout, error)
}

// RunSynthesisAgent combines complete reviews and validates source references.
// The returned text preserves the adapter response even when validation fails.
// It uses the same prompt preparation and read-capable invocation as ordinary reviews.
func RunSynthesisAgent(
	ctx context.Context,
	a agent.Agent,
	reviews []ReviewResult,
	prompt, minSeverity string,
	out io.Writer,
	hooks SynthesisHooks,
) (SynthesisDocument, string, error) {
	resolveCheckout := func() (SynthesisCheckout, error) {
		if hooks.Checkout == nil {
			return SynthesisCheckout{}, nil
		}
		checkout, err := hooks.Checkout()
		if err != nil {
			if checkout.Cleanup != nil {
				checkout.Cleanup()
			}
			return SynthesisCheckout{}, &SynthesisCheckoutError{Err: err}
		}
		return checkout, nil
	}

	checkout, err := resolveCheckout()
	if err != nil {
		return SynthesisDocument{}, "", err
	}
	if checkout.Cleanup != nil {
		defer checkout.Cleanup()
	}
	configRepoPath := checkout.ConfigRepoPath
	if configRepoPath == "" {
		configRepoPath = hooks.ConfigRepoPath
	}
	builder := promptpkg.NewBuilderWithConfig(nil, hooks.GlobalConfig).ForRepo(configRepoPath, 0)
	prepared, err := builder.Prepare(prompt, promptpkg.SnapshotTarget{
		RepoPath: checkout.RepoPath, ConfigRepoPath: configRepoPath,
		IgnoreRepoConfig: hooks.IgnoreRepoConfig,
	})
	if err != nil {
		return SynthesisDocument{}, "", &SynthesisCheckoutError{Err: err}
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	prompt = prepared.Prompt
	a, cleanup, err := agent.PrepareReviewToolPolicy(a, hooks.Tools, hooks.GlobalConfig)
	if err != nil {
		return SynthesisDocument{}, "", err
	}
	defer cleanup()
	if err := agent.CheckStrictReviewPrepared(hooks.Tools, a); err != nil {
		return SynthesisDocument{}, "", err
	}

	if hooks.BeforeInvoke != nil {
		if err := hooks.BeforeInvoke(ctx); err != nil {
			return SynthesisDocument{}, "", err
		}
	}
	output, err := invokeReview(ctx, a, checkout.RepoPath, checkout.GitRef, prompt, SynthesisSchema, out)
	doc, err := decodeSynthesisResult(a, reviews, jsontext.Value(output), err)
	if err != nil {
		return SynthesisDocument{}, output, err
	}
	settleFindings(ctx, checkout.RepoPath, checkout.GitRef, &doc)
	return doc, output, nil
}

type synthesisFindingRef struct {
	Review  int `json:"review"`
	Finding int `json:"finding"`
}

func decodeSynthesisResult(a agent.Agent, reviews []ReviewResult, raw jsontext.Value, err error) (SynthesisDocument, error) {
	if err != nil {
		return SynthesisDocument{}, err
	}
	document, err := agentReviewJSON(string(raw))
	if err != nil {
		return SynthesisDocument{}, err
	}
	var wire map[string]jsontext.Value
	if err := json.Unmarshal(document, &wire); err != nil {
		return SynthesisDocument{}, err
	}
	var findings []map[string]jsontext.Value
	if err := json.Unmarshal(wire["findings"], &findings); err != nil {
		return SynthesisDocument{}, err
	}
	refs := make([][]synthesisFindingRef, len(findings))
	for i, finding := range findings {
		if len(finding["source_findings"]) == 0 {
			return SynthesisDocument{}, fmt.Errorf("synthesis finding %d is missing source_findings", i+1)
		}
		if string(finding["source_findings"]) == "null" {
			return SynthesisDocument{}, fmt.Errorf("synthesis finding %d has null source_findings", i+1)
		}
		if err := json.Unmarshal(finding["source_findings"], &refs[i]); err != nil {
			return SynthesisDocument{}, fmt.Errorf("synthesis finding %d: %w", i+1, err)
		}
		delete(finding, "source_findings")
	}
	wire["findings"], err = json.Marshal(findings)
	if err != nil {
		return SynthesisDocument{}, err
	}
	document, err = json.Marshal(wire)
	if err != nil {
		return SynthesisDocument{}, err
	}
	doc, err := DecodeSynthesisDocument(document, reviews)
	if err != nil {
		return SynthesisDocument{}, err
	}
	if err := validateLiveDocument(a.Name(), doc); err != nil {
		return SynthesisDocument{}, err
	}
	for i := range doc.Findings {
		finding := &doc.Findings[i]
		for _, ref := range refs[i] {
			if ref.Review < 1 || ref.Review > len(reviews) || !slices.Contains(finding.Sources, ref.Review) {
				return SynthesisDocument{}, fmt.Errorf("synthesis finding %d has invalid source review %d", i+1, ref.Review)
			}
			source := reviews[ref.Review-1].structuredReview()
			if source == nil || source.Legacy != nil || ref.Finding < 1 || ref.Finding > len(source.Findings) {
				return SynthesisDocument{}, fmt.Errorf("synthesis finding %d has invalid source finding %d in review %d", i+1, ref.Finding, ref.Review)
			}
			original := source.Findings[ref.Finding-1]
			finding.Unlikely = finding.Unlikely || original.Unlikely
			if original.Topic == "security" {
				finding.Topic = "security"
			}
		}
		for _, source := range finding.Sources {
			if original := reviews[source-1].structuredReview(); original != nil && len(original.Findings) > 0 && !slices.ContainsFunc(refs[i], func(ref synthesisFindingRef) bool { return ref.Review == source }) {
				return SynthesisDocument{}, fmt.Errorf("synthesis finding %d is missing a finding reference for review %d", i+1, source)
			}
		}
	}
	// minSeverity never removes findings; callers apply it when rendering
	// and deriving the verdict.
	return doc, nil
}
