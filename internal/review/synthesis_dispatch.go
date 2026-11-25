package review

import (
	"context"
	"encoding/json/jsontext"
	"io"

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

func decodeSynthesisResult(a agent.Agent, reviews []ReviewResult, raw jsontext.Value, err error) (SynthesisDocument, error) {
	if err != nil {
		return SynthesisDocument{}, err
	}
	document, err := agentReviewJSON(string(raw))
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
	// minSeverity never removes findings; callers apply it when rendering
	// and deriving the verdict.
	return doc, nil
}
