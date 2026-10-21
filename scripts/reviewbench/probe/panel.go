package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// runPanel reviews with a configured roborev panel the way the daemon does:
// every member reviews the change, then one synthesis call combines their
// findings. A single surviving member passes through unchanged, and a panel
// whose members all found nothing reports nothing without a synthesis call.
// With memberOut set, each member's own findings are written there as well,
// so members can be judged on their own. With membersFrom set, members are not
// run: each one's review comes from the first of those directories that holds
// MEMBER/review.json, so synthesis can be compared on the same member reviews.
func (p probe) runPanel(ctx context.Context, name, memberOut string, membersFrom []string, out string) error {
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
		if len(membersFrom) > 0 {
			results[i], errs[i] = loadMemberReview(membersFrom, m)
			continue
		}
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
		// Reused members never create the directory, and a panel whose members
		// found nothing skips synthesis, which otherwise would.
		if err := os.MkdirAll(p.artifacts, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p.artifacts, "review.json"), document, 0o644); err != nil {
			return err
		}
	}
	return p.writeFindings(out, document, time.Since(started))
}

// loadMemberReview reads a member's saved review document and Markdown from
// the first directory that has them.
func loadMemberReview(dirs []string, m config.ResolvedMember) (review.ReviewResult, error) {
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, m.Name, "review.json")); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		result, _, err := loadReview(filepath.Join(dir, m.Name), m.Agent, m.ReviewType)
		return result, err
	}
	return review.ReviewResult{}, fmt.Errorf("no saved review for member %s", m.Name)
}

// loadReview reads a saved review document and its Markdown from dir, with
// the time the review took when the review store recorded it.
func loadReview(dir, agentName, reviewType string) (review.ReviewResult, time.Duration, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "review.json"))
	if err != nil {
		return review.ReviewResult{}, 0, err
	}
	doc, err := structuredreview.Decode(raw)
	if err != nil {
		return review.ReviewResult{}, 0, fmt.Errorf("decode %s: %w", dir, err)
	}
	output, err := os.ReadFile(filepath.Join(dir, "review.md"))
	if err != nil {
		return review.ReviewResult{}, 0, err
	}
	var elapsed time.Duration
	if metaRaw, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		var m sampleMeta
		if json.Unmarshal(metaRaw, &m) == nil {
			elapsed = time.Duration(m.ElapsedSeconds * float64(time.Second))
		}
	}
	return review.ReviewResult{
		Agent: agentName, ReviewType: reviewType, Status: review.ResultDone,
		Output: string(output), Structured: &doc, StructuredOutput: raw,
	}, elapsed, nil
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

	if p.synthesis == "concat" {
		return concatenate(succeeded)
	}
	ids := p.synthesis == "ids" || p.synthesis == "ids-text"
	text := review.BuildSynthesisPrompt(succeeded, "low")
	if ids {
		if p.synthesisInstructions == "" {
			return nil, fmt.Errorf("-synthesis %s needs -synthesis-instructions", p.synthesis)
		}
		text = strings.TrimSpace(p.synthesisInstructions) + "\n\n" + renderWithIDs(succeeded)
	} else if p.synthesisInstructions != "" {
		var err error
		if text, err = replaceInstructions(text, p.synthesisInstructions); err != nil {
			return nil, err
		}
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
	if p.artifacts != "" {
		if err := os.WriteFile(filepath.Join(p.artifacts, "synthesis-prompt.md"), []byte(text), 0o644); err != nil {
			return nil, err
		}
	}
	doc, err := review.RunSynthesisAgent(ctx, a, succeeded, text, "low", log,
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
	if ids {
		var restored, invented int
		doc, restored, invented = accountForIDs(doc, succeeded, p.synthesis == "ids")
		fmt.Fprintf(os.Stderr, "probe: ids synthesis restored %d omitted member finding(s), dropped %d uncited\n", restored, invented)
	}
	return json.Marshal(doc)
}

// replaceInstructions swaps the instructions at the top of a synthesis prompt
// for an experimental set, keeping the member reviews that follow them.
func replaceInstructions(prompt, instructions string) (string, error) {
	i := strings.Index(prompt, "---\n### Review 1")
	if i < 0 {
		return "", fmt.Errorf("synthesis prompt has no review section")
	}
	return strings.TrimSpace(instructions) + "\n\n" + prompt[i:], nil
}

// concatenate is the no-synthesis reference: every member finding, unmerged,
// citing the review it came from.
func concatenate(reviews []review.ReviewResult) ([]byte, error) {
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "All member findings, unmerged."}
	for i, r := range reviews {
		if r.Structured == nil {
			continue
		}
		for _, f := range r.Structured.Findings {
			f.Sources = []int{i + 1}
			doc.Findings = append(doc.Findings, f)
		}
	}
	doc.Verdict = structuredreview.VerdictPass
	if len(doc.Findings) > 0 {
		doc.Verdict = structuredreview.VerdictFail
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
