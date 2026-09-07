package review

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	promptpkg "go.kenn.io/roborev/internal/prompt"
)

// SynthesisCheckout is the reviewed checkout a plain review agent needs to
// verify findings. Cleanup may be nil.
type SynthesisCheckout struct {
	RepoPath string
	GitRef   string
	Cleanup  func()
}

// SynthesisCheckoutError wraps a failure to prepare the checkout for the
// plain review fallback so callers can retry it as infrastructure rather than
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
// Oversized inputs use repo-local files and a read-capable review invocation;
// inline inputs may use tool-free classifier or synthesis entry points.
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

	// The configured inline budget chooses transport, never which findings survive.
	// File inputs need the review interface because classifier/synthesis entry
	// points may disable filesystem tools entirely.
	if len(prompt) > config.ResolveMaxPromptSize(hooks.ConfigRepoPath, hooks.GlobalConfig) {
		checkout, err := resolveCheckout()
		if err != nil {
			return SynthesisDocument{}, err
		}
		if checkout.Cleanup != nil {
			defer checkout.Cleanup()
		}
		builder := promptpkg.NewBuilder(nil).ForRepo(checkout.RepoPath, 0)
		files := make([]string, 0, len(reviews))
		for _, r := range reviews {
			// Keep the same status handling and threshold-free rendering as inline inputs.
			content := synthesisReviewContent(r)
			file, cleanup, err := builder.WriteSynthesisReviewSnapshot(content, promptpkg.SnapshotTarget{
				ConfigRepoPath: hooks.ConfigRepoPath,
			})
			if err != nil {
				return SynthesisDocument{}, &SynthesisCheckoutError{Err: fmt.Errorf("write synthesis review: %w", err)}
			}
			defer cleanup()
			files = append(files, file)
		}
		prompt = buildSynthesisPrompt(reviews, files)
		invoke()
		var raw json.RawMessage
		if sa, ok := a.(agent.StructuredReviewAgent); ok {
			raw, err = sa.ReviewWithSchema(ctx, checkout.RepoPath, checkout.GitRef, prompt, SynthesisSchema, out)
		} else {
			var result string
			result, err = a.Review(ctx, checkout.RepoPath, checkout.GitRef, prompt, out)
			raw = json.RawMessage(result)
		}
		return decodeSynthesisResult(a, reviews, raw, err)
	}

	var raw json.RawMessage
	var err error
	switch sa := a.(type) {
	case agent.SchemaAgent:
		invoke()
		raw, err = sa.ClassifyWithSchema(ctx, "", "", prompt, SynthesisSchema, out)
	case agent.StructuredReviewAgent:
		// Codex and similar agents constrain review output to a schema but
		// expose no classifier entry point.
		checkout, cerr := resolveCheckout()
		if cerr != nil {
			return SynthesisDocument{}, cerr
		}
		if checkout.Cleanup != nil {
			defer checkout.Cleanup()
		}
		invoke()
		raw, err = sa.ReviewWithSchema(ctx, checkout.RepoPath, checkout.GitRef, prompt, SynthesisSchema, out)
	case agent.SynthesisAgent:
		invoke()
		raw, err = sa.Synthesize(ctx, prompt, out)
	default:
		checkout, cerr := resolveCheckout()
		if cerr != nil {
			return SynthesisDocument{}, cerr
		}
		if checkout.Cleanup != nil {
			defer checkout.Cleanup()
		}
		invoke()
		var output string
		output, err = a.Review(ctx, checkout.RepoPath, checkout.GitRef, prompt, out)
		raw = json.RawMessage(output)
	}
	return decodeSynthesisResult(a, reviews, raw, err)
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
