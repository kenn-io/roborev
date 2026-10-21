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

To compare agent harnesses instead of code, run one ref through several
agents with the same model. Each `--arm` is `AGENT:PROVIDER:MODEL`, and
PROVIDER only applies to pi:

```bash
scripts/reviewbench/eval.sh harness --ref HEAD --name <topic> \
  --arm codex::gpt-6.1-sol --arm pi:cpa:gpt-6.1-sol [--provider P --judge-model M]
```

To compare a roborev panel with its members and a stock reviewer, run the
panel from your roborev config. It judges the panel and each member on its own:

```bash
scripts/reviewbench/eval.sh panel --ref HEAD --name <topic> --panel <panel> \
  --baseline <earlier stock run> [--panel-config FILE] [--provider P --judge-model M]
```

Only the `[review…]` sections of the panel config reach the eval.

Runs take an hour or more. Start them detached from the agent session, so a
restart of the agent or its host app does not kill them. On Linux with a
systemd user manager, write the commands to a script under
`~/.local/share/roborev-reviewbench/queue/` and start it as a transient unit
from this repository's root:

```bash
systemd-run --user --unit=reviewbench-<topic> --collect --same-dir \
  --setenv=PATH="$PATH" --setenv=HOME="$HOME" \
  ~/.local/share/roborev-reviewbench/queue/<topic>.sh
systemctl --user status reviewbench-<topic>   # progress: tail the run's log
```

A unit runs in its own cgroup, so it outlives the session; with lingering on
(`loginctl enable-linger`), it also outlives logout. Without systemd, use
`setsid nohup <script> >log 2>&1 &`. Do not run an eval as a background job of
the agent's shell: it dies with the session.

Each run builds its probe in a temporary worktree of this repository, and two
builds at once collide on the repository lock. Runs can overlap once built:
start the next run only after the previous one logs `run NAME: roborev ...`.

A stopped or partly failed run resumes when the same `run` command is run
again. `scripts/reviewbench/eval.sh compare <name>-base <name>-head` reprints
the table.

## Improve a panel step by step

Synthesis lets focused reviewers be tried one at a time:

1. Add the reviewer as a subagent in a panel config, with a one-member panel
   such as `focus_<name>`. Keep all experiment panels in one config file.
2. Run the one-member panel. It reports the reviewer's findings without
   synthesis, and its member reviews are saved under `runs/<name>/artifacts`.
3. Check what the reviewer adds beyond the current panel, and its augmented
   precision (AP) from `compare`:

   ```bash
   scripts/reviewbench/eval.sh coverage <panel run> <focus run>
   ```

4. Rerun only the synthesis, with and without the reviewer, on the same saved
   member reviews. `--members-from` takes each member's review from the first
   listed run that has it, so no member runs again:

   ```bash
   scripts/reviewbench/eval.sh panel --ref HEAD --name <topic>-without \
     --panel <panel> --members-from <panel run> --panel-config FILE ...
   scripts/reviewbench/eval.sh panel --ref HEAD --name <topic>-with \
     --panel <panel with reviewer> --members-from <panel run> \
     --members-from <focus run> --panel-config FILE ...
   ```

   Synthesizing both arms fresh keeps synthesis noise in both.

5. Keep a reviewer only when the panel gains recall without a matching loss in
   AP. Then fold it into the main panel, or into the default prompt when it
   helps every member.

The same reuse makes synthesis prompt changes cheap to measure: build the new
synthesis at a new ref and rerun only the synthesis on saved member reviews.
`coverage` between the panel and each member shows how many golden findings
members found that synthesis dropped.

Do not add reviewers for problems that compilers, type checkers, or the
project's linters catch, even when the benchmark counts them.

## How it stays isolated

- Each ref's review code is built in a temporary worktree, so the prompts,
  schema, and post-processing under test are that ref's own.
- Reviews run with an empty `ROBOREV_DATA_DIR`. They never touch the user's
  daemon, database, or roborev config.
- Results (findings, scores, logs, and reports) live in
  `~/.local/share/roborev-reviewbench` (or `ROBOREV_EVAL_DIR`). Benchmark
  repositories and checkouts are disposable and live under
  `${TMPDIR:-/tmp}/roborev-reviewbench` (or `ROBOREV_EVAL_CACHE`). Neither is in
  this repository.
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
  `~/.local/share/roborev-reviewbench/runs/<name>/scoring/results.details.json`
  before concluding. The review prompt, document, and agent log for each pull
  request are under `runs/<name>/artifacts/`.
- Do not tune toward the benchmark. Its golden set is mostly earlier AI
  reviewers' findings, and it rewards volume. A change that only adds findings
  can raise recall without making reviews more useful.

Save any HTML report under `~/.local/share/roborev-reviewbench/reports/`,
named by date and topic. A saved report is opened outside T3, so it must carry
its own colors and fonts: give every theme variable a fallback, such as
`var(--foreground, var(--fb-foreground))`, with light and dark `--fb-*` values
defined in the page. Keep run artifacts out of the repository, commits,
and pull request text.
