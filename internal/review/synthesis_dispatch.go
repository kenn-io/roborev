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
// holds even if the agent ignored the instruction. Schema and synthesis agents
// run without a checkout; plain review agents run against the checkout
// returned by hooks.Checkout.
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

	var raw json.RawMessage
	var err error
	switch sa := a.(type) {
	case agent.SchemaAgent:
		invoke()
		raw, err = sa.ClassifyWithSchema(ctx, "", "", prompt, SynthesisSchema, out)
	case agent.SynthesisAgent:
		invoke()
		raw, err = sa.Synthesize(ctx, prompt, out)
	default:
		var checkout SynthesisCheckout
		if hooks.Checkout != nil {
			checkout, err = hooks.Checkout()
			if checkout.Cleanup != nil {
				defer checkout.Cleanup()
			}
			if err != nil {
				return SynthesisDocument{}, &SynthesisCheckoutError{Err: err}
			}
		}
		invoke()
		var output string
		output, err = a.Review(ctx, checkout.RepoPath, checkout.GitRef, prompt, out)
		raw = json.RawMessage(output)
	}
	if err != nil {
		return SynthesisDocument{}, err
	}
	doc, err := DecodeSynthesisDocument(raw, reviews)
	if err != nil {
		return SynthesisDocument{}, err
	}
	return doc.Filter(minSeverity), nil
}
