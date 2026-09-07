package review

import (
	"context"
	"encoding/json"
	"io"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	promptpkg "go.kenn.io/roborev/internal/prompt"
)

// SynthesisCheckout is the checkout used for agent execution and prompt file
// handoff. Cleanup may be nil.
type SynthesisCheckout struct {
	RepoPath string
	GitRef   string
	Cleanup  func()
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
	// ConfigRepoPath resolves the prompt budget and snapshot directory from the trusted checkout.
	ConfigRepoPath string
	GlobalConfig   *config.Config
	// BeforeInvoke runs once, immediately before the agent is called. The
	// daemon uses it to record that an agent actually ran, so it must fire
	// after any checkout preparation that could still fail.
	BeforeInvoke func()
	// Checkout resolves where read-capable agents run, including when
	// oversized synthesis inputs need repo-local files.
	Checkout func() (SynthesisCheckout, error)
}

// RunSynthesisAgent combines complete reviews and validates source references.
// It uses the same prompt preparation and read-capable invocation as ordinary reviews.
func RunSynthesisAgent(
	ctx context.Context,
	a agent.Agent,
	reviews []ReviewResult,
	prompt, minSeverity string,
	out io.Writer,
	hooks SynthesisHooks,
) (SynthesisDocument, error) {
	invoke := func() {
		if hooks.BeforeInvoke != nil {
			hooks.BeforeInvoke()
		}
	}

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
		return SynthesisDocument{}, err
	}
	if checkout.Cleanup != nil {
		defer checkout.Cleanup()
	}
	builder := promptpkg.NewBuilderWithConfig(nil, hooks.GlobalConfig).ForRepo(hooks.ConfigRepoPath, 0)
	prepared, err := builder.Prepare(prompt, promptpkg.SnapshotTarget{
		RepoPath: checkout.RepoPath, ConfigRepoPath: hooks.ConfigRepoPath,
	})
	if err != nil {
		return SynthesisDocument{}, &SynthesisCheckoutError{Err: err}
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	prompt = prepared.Prompt

	invoke()
	output, err := invokeReview(ctx, a, checkout.RepoPath, checkout.GitRef, prompt, SynthesisSchema, out)
	return decodeSynthesisResult(a, reviews, json.RawMessage(output), err)
}

func decodeSynthesisResult(a agent.Agent, reviews []ReviewResult, raw json.RawMessage, err error) (SynthesisDocument, error) {
	if err != nil {
		return SynthesisDocument{}, err
	}
	if noVerdict := NoVerdict(string(raw)); noVerdict != nil {
		return SynthesisDocument{}, noVerdict
	}
	doc, err := DecodeSynthesisDocument(raw, reviews)
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
