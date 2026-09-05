package review

import (
	"context"
	"encoding/json"
	"io"

	"go.kenn.io/roborev/internal/agent"
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
	// BeforeInvoke runs once, immediately before the agent is called. The
	// daemon uses it to record that an agent actually ran, so it must fire
	// after any checkout preparation that could still fail.
	BeforeInvoke func()
	// Checkout resolves where a plain review agent runs. It is called only
	// when the agent implements neither SchemaAgent nor SynthesisAgent.
	Checkout func() (SynthesisCheckout, error)
}

// RunSynthesisAgent sends the synthesis prompt to the most capable interface
// the agent implements, decodes the schema-validated document against the
// reviews it combined, and drops findings below minSeverity so the threshold
// holds even if the agent ignored the instruction. Classifier and synthesis
// agents run without a checkout; schema-constrained and plain review agents
// run against the checkout returned by hooks.Checkout.
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
	return doc.Filter(minSeverity), nil
}
