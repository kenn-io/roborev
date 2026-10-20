---
name: reviewbench-eval
description: Measure whether a change to roborev's review prompts or review pipeline changes review quality, by running ReviewBench on the code before and after the change. Costly; run only when the user explicitly invokes it.
disable-model-invocation: true
---

# ReviewBench eval

Compare review quality between two roborev git refs on ReviewBench's 25
pull request test set. Use it after changing review prompts, the review
schema, synthesis, or finding post-processing.

Invoking this skill authorizes the reviews and judge calls it runs, for this
eval only. It does not authorize `roborev review` on any other code, and it
must never run from CI, hooks, or another skill.

## Cost

- Each arm runs one agent review per pull request: 25 per arm, 50 for an A/B
  comparison. Expect about an hour with four reviews at a time.
- Judging each arm calls the judge model once per pull request plus once per
  finding that matches nothing.
- Tell the user the plan, the refs, the agent, and the model before starting.

## Before running

1. The eval reviews committed code only. Commit the change under test first.
2. Pick the refs. The base is usually `origin/main` or the branch's merge base;
   the head is usually `HEAD`. Both arms must use the same agent, model, and
   reasoning level, or the comparison measures the model instead of the
   change.
3. Check the tools: `go`, `git`, `jq`, and Node.js 20 or newer.
4. Pick a judge both arms will share, and record it with the results. The
   leaderboard judges with `anthropic/claude-sonnet-5`; any judge works for a
   before-and-after comparison, as long as both arms use it.

## Judge login

The judge uses the same pi login, settings, and extensions as the `pi`
command, so any model `pi --provider P --model M -p 'hi'` can reach works as
the judge. Setup patches ReviewBench's judge from pi 0.75.5 to pi 1.x
(`scripts/reviewbench/reviewbench-pi1.patch`); the old pi cannot see providers
that pi extensions register, such as a local proxy.

Check the judge model answers before a long run. A key can be limited to some
models; "model is not allowed for this API key" means pick another. Never
print pi or proxy configuration files that hold keys.

## Run

Smoke-test one pull request when the pipeline itself changed:

```bash
scripts/reviewbench/eval.sh run --ref HEAD --name smoke --pr 9 --jobs 1
scripts/reviewbench/eval.sh judge --name smoke [--provider P --judge-model M]
```

Then run the comparison in the background. It runs both arms, judges both,
and prints the comparison:

```bash
scripts/reviewbench/eval.sh ab --base origin/main --head HEAD --name <topic> \
  [--provider P --judge-model M]
```

A stopped or partly failed run resumes when the same `run` command is run
again. `scripts/reviewbench/eval.sh compare <name>-base <name>-head` reprints
the table.

## How it stays isolated

- Each ref's review code is built in a temporary worktree, so the prompts,
  schema, and post-processing under test are that ref's own.
- Reviews run with an empty `ROBOREV_DATA_DIR`. They never touch the user's
  daemon, database, or roborev config.
- Benchmark repositories and results live under
  `${TMPDIR:-/tmp}/roborev-reviewbench` (or `ROBOREV_EVAL_DIR`), never in this
  repository.
- Codex runs ignore user config, so connectors and web search cannot fetch the
  upstream pull request's review comments, which are the answers. Claude Code
  reviews only get read tools.

## Report

Lead with the comparison table, then say what it means:

- One round on 25 pull requests is noisy. Treat a difference of a few points
  as noise unless it repeats in a second round or shows in the per-PR
  changes.
- Grounded recall (GR) is the share of golden findings the review found.
  Grounded precision (GP) ignores findings that match nothing, so it rarely
  moves; augmented precision (AP) shows whether new findings are valid.
- Read a few per-PR changes in
  `${TMPDIR:-/tmp}/roborev-reviewbench/runs/<name>/scoring/results.details.json`
  before concluding. The review prompt, document, and agent log for each pull
  request are under `runs/<name>/artifacts/`.
- Do not tune toward the benchmark. Its golden set is mostly earlier AI
  reviewers' findings, and it rewards volume. A change that only adds findings
  can raise recall without making reviews more useful.

Keep run artifacts out of the repository, commits, and pull request text.
