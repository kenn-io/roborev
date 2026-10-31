package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

type ciRewriteFinding struct {
	Finding int    `json:"finding"`
	Problem string `json:"problem,omitempty"`
	Fix     string `json:"fix"`
}

var (
	ciRewriteSchema    = jsontext.Value(`{"type":"object","additionalProperties":false,"required":["findings"],"properties":{"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["finding","problem","fix"],"properties":{"finding":{"type":"integer"},"problem":{"type":"string"},"fix":{"type":"string"}}}}}}`)
	ciRewriteFixSchema = jsontext.Value(`{"type":"object","additionalProperties":false,"required":["findings"],"properties":{"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["finding","fix"],"properties":{"finding":{"type":"integer"},"fix":{"type":"string"}}}}}}`)
)

func (wp *WorkerPool) rewriteCIFindings(ctx context.Context, job *storage.ReviewJob, cfg ciCheckConfig, checked structuredreview.Document, notes ciCheckResponse, a *agent.CodexAgent, repo string, out io.Writer) (structuredreview.Document, error) {
	selected := make(map[int]int)
	var input strings.Builder
	picked := notes.Verdicts[:0:0]
	for i, f := range checked.Findings {
		if config.SeverityRank(f.Severity) < config.SeverityRank(job.MinSeverity) {
			continue
		}
		id := notes.Kept[i]
		var evidence, reason string
		var match int
		count := 0
		for k, note := range notes.Verdicts {
			if note.Finding == id {
				count++
				evidence, reason, match = note.Evidence, note.Reason, k
			}
		}
		if count != 1 || strings.TrimSpace(evidence) == "" || strings.TrimSpace(reason) == "" {
			return checked, fmt.Errorf("finding %d has incomplete checker notes", id)
		}
		selected[id] = i
		picked = append(picked, notes.Verdicts[match])
		fmt.Fprintf(&input, "## Finding %d (%s)\n\n- `%s`: %s\n\n  **Fix:** %s\n\n", id, f.Severity, f.Location, f.Problem, f.Fix)
	}
	if len(selected) == 0 {
		return checked, nil
	}
	originalText := input.String()
	verdicts, err := json.Marshal(picked, jsontext.WithIndent("  "))
	if err != nil {
		return checked, err
	}
	firstPrompt := strings.NewReplacer("{{STYLE}}", ciRewriteStyle, "{{BODY}}", cfg.Title+"\n\n"+cfg.Description, "{{FINDINGS}}", originalText+"\n\n## Checker notes, from running the code in a sandbox\n\n```json\n"+string(verdicts)+"\n```").Replace(ciRewritePrompt)
	var first []ciRewriteFinding
	for phase := range 2 {
		text, schema := firstPrompt, ciRewriteSchema
		if phase == 1 {
			encoded, err := json.Marshal(first)
			if err != nil {
				return checked, err
			}
			text = strings.NewReplacer("{{FINDINGS}}", string(encoded), "{{ORIGINAL}}", originalText).Replace(ciRewriteFixPrompt)
			schema = ciRewriteFixSchema
		}
		fmt.Fprintf(out, "CI rewrite phase %d\n", phase+1)
		prepared, err := prompt.NewBuilderWithConfig(wp.db, wp.cfgGetter.Config()).Prepare(text, prompt.SnapshotTarget{RepoPath: repo, ConfigRepoPath: job.RepoPath})
		if err != nil {
			return checked, err
		}
		raw, err := a.ReviewWithSchema(ctx, repo, job.GitRef, prepared.Prompt, schema, out)
		if prepared.Cleanup != nil {
			prepared.Cleanup()
		}
		if err != nil {
			return checked, err
		}
		var response struct {
			Findings []ciRewriteFinding `json:"findings"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return checked, err
		}
		seen := make(map[int]bool)
		for _, f := range response.Findings {
			_, exists := selected[f.Finding]
			if !exists || seen[f.Finding] || strings.TrimSpace(f.Fix) == "" || phase == 0 && strings.TrimSpace(f.Problem) == "" {
				return checked, fmt.Errorf("invalid rewrite finding %d in phase %d", f.Finding, phase+1)
			}
			seen[f.Finding] = true
		}
		if len(seen) != len(selected) {
			return checked, fmt.Errorf("incomplete rewrite phase %d", phase+1)
		}
		if phase == 0 {
			first = response.Findings
			continue
		}
		rewritten := checked
		rewritten.Findings = slices.Clone(checked.Findings)
		for _, f := range first {
			rewritten.Findings[selected[f.Finding]].Problem = f.Problem
		}
		for _, f := range response.Findings {
			rewritten.Findings[selected[f.Finding]].Fix = f.Fix
		}
		return rewritten, nil
	}
	return checked, nil
}

const ciRewriteStyle = `Write each finding for the PR's author. They know what the change is for, but may not know this part of the code, its names, or its terms.

- First, what goes wrong for a person: what they do and what they see. Example: "If you click Resync while a sync is running, the page says the sync failed instead of saying it's busy."
- Then why, in one short sentence that describes what the code does instead of naming it. Name at most one function, file, or setting, and only when the author needs it to find the spot.
- Then who runs into it and how often: every user on every run, anyone who does X, only when Y, only in tests, or "can't tell". Use the checker's notes below, which come from running the code, and check the callers and settings yourself. Say plainly when it's rare or never happens in practice.
- End with **Fix:** and one sentence on what to change. Here, name every function, test, flag, or file the author has to touch, and every place that needs the change.
- Before the fix, explain any term a newcomer to this repo wouldn't know, or replace it. Keep the original's claims; add no facts except who and how often.
- Keep each finding under 60 words, not counting its location.
`

const ciRewritePrompt = `You are the final step of a code review. Reviewers already found the defects below in this repository's current checkout, a pull request's head. Rewrite each finding following the style rules. Read the code as much as you need to describe each problem in terms of what someone sees. Do not build the project, run tests, or execute code.

Keep each finding's claim, severity, and location exactly. Add no new problems, drop none, and don't judge whether a finding is right or worth fixing.

Return JSON with findings: [{"finding": N, "problem": "text before Fix", "fix": "fix sentence"}]. Put every pre-Fix style element in problem, including who is affected, the conditions they must meet, and how often; omit severity, location, and the Fix label. The fix field contains only the sentence, without a Fix label. Preserve the input finding numbers.

## Style rules

{{STYLE}}

## Pull request

{{BODY}}

## Findings to rewrite

{{FINDINGS}}
`

const ciRewriteFixPrompt = `Read the code as much as needed. Do not build the project, run tests, or execute code.

Below are code review findings, already rewritten in plain words for the PR's author. Rewrite only each finding's **Fix:** line. Leave everything before **Fix:** as it is.

- **Fix:** one sentence on what to change, in plain words, with no function, test, flag, field, or file names.
- Then **Where:** and list every function, test, flag, or file the author has to touch, in backticks, separated by commas. Take the names from the current fix line and the original finding. Nothing else goes on that line.

Return JSON with findings: [{"finding": N, "fix": "plain sentence **Where:** names"}]. Keep Where on the same line as the sentence. Preserve the input finding numbers.

## Findings

{{FINDINGS}}

## Original findings, for the names

{{ORIGINAL}}
`
