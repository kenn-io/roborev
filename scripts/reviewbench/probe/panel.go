package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/review"
)

// runPanel reviews with a configured roborev panel the way the daemon does:
// every member reviews the change, then one synthesis call combines their
// findings. A single surviving member passes through unchanged, and a panel
// whose members all found nothing reports nothing without a synthesis call.
// With memberOut set, each member's own findings are written there as well,
// so members can be judged on their own.
func (p probe) runPanel(ctx context.Context, name, memberOut, out string) error {
	members, synth, err := config.ResolvePanel(name, p.repo, p.cfg)
	if err != nil {
		return err
	}
	if memberOut != "" {
		if err := os.MkdirAll(memberOut, 0o755); err != nil {
			return err
		}
	}

	started := time.Now()
	results := make([]review.ReviewResult, len(members))
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Go(func() {
			r := reviewer{
				name: m.Name, agent: m.Agent, model: m.Model, provider: m.Provider,
				reasoning: m.Reasoning, reviewType: m.ReviewType, instructions: m.Instructions, panel: name,
			}
			result, elapsed, err := p.review(ctx, r, p.memberArtifacts(m.Name))
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = result
			if memberOut != "" {
				errs[i] = p.writeFindings(filepath.Join(memberOut, m.Name+".json"), result.StructuredOutput, elapsed)
			}
		})
	}
	wg.Wait()

	var succeeded []review.ReviewResult
	var failures []string
	for i, m := range members {
		if errs[i] != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", m.Name, errs[i]))
			continue
		}
		succeeded = append(succeeded, results[i])
	}
	if len(failures) > 0 {
		// A failed member would silently make the panel look weaker or
		// stronger than it is, so the whole pull request fails and is retried.
		return fmt.Errorf("panel members failed: %s", strings.Join(failures, "; "))
	}

	document, err := p.synthesize(ctx, synth, succeeded)
	if err != nil {
		return err
	}
	if p.artifacts != "" {
		if err := os.WriteFile(filepath.Join(p.artifacts, "review.json"), document, 0o644); err != nil {
			return err
		}
	}
	return p.writeFindings(out, document, time.Since(started))
}

func (p probe) memberArtifacts(member string) string {
	if p.artifacts == "" {
		return ""
	}
	return filepath.Join(p.artifacts, "members", member)
}

func (p probe) synthesize(ctx context.Context, synth config.SynthesisSpec, succeeded []review.ReviewResult) ([]byte, error) {
	if len(succeeded) == 1 {
		return succeeded[0].StructuredOutput, nil
	}
	anyFindings := false
	for _, r := range succeeded {
		if r.Structured != nil && len(r.Structured.Findings) > 0 {
			anyFindings = true
		}
	}
	if !anyFindings {
		return []byte(`{"findings":[]}`), nil
	}

	a, err := configure(reviewer{agent: synth.Agent, model: synth.Model, reasoning: synth.Reasoning}, false)
	if err != nil {
		return nil, err
	}
	log, closeLog, err := p.synthesisLog()
	if err != nil {
		return nil, err
	}
	defer closeLog()
	doc, err := review.RunSynthesisAgent(ctx, a, succeeded, review.BuildSynthesisPrompt(succeeded, "low"), "low", log,
		review.SynthesisHooks{
			ConfigRepoPath: p.repo,
			GlobalConfig:   p.cfg,
			Checkout: func() (review.SynthesisCheckout, error) {
				return review.SynthesisCheckout{RepoPath: p.repo, ConfigRepoPath: p.repo, GitRef: p.gitRef}, nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("synthesis: %w", err)
	}
	return json.Marshal(doc)
}

func (p probe) synthesisLog() (*os.File, func(), error) {
	if p.artifacts == "" {
		devNull, err := os.Open(os.DevNull)
		return devNull, func() { _ = devNull.Close() }, err
	}
	if err := os.MkdirAll(p.artifacts, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.Create(filepath.Join(p.artifacts, "synthesis.log"))
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}
