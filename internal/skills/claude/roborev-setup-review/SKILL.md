---
name: roborev-setup-review
description: Use only when the user explicitly invokes /roborev-setup-review
disable-model-invocation: true
---

# roborev-setup-review

Work with the user to design a review for their repository, then write the
roborev configuration and rubrics that run it. The material can be code or
anything else committed to Git: a legal brief, a contract, a research report, a
manual. Find out what the reviewer should catch and why, play it back, and
write files only after the user approves.

## Usage

```text
/roborev-setup-review [what the review is for]
```

## Explicit invocation only

Invocation must be explicit: literal personal `/roborev-setup-review`, or structured
Claude Code skill selection.
Requests such as “set up a legal review for this repository” without one of
these explicit mechanisms must use native behavior and must not run roborev.

Honor existing session authorization and project CLAUDE.md instructions. This
invocation authorizes read-only inspection and the conversation. Writing
repository files, changing global configuration, installing hooks, committing,
and running a review each still need the user's approval.

## Sandbox access

roborev uses a local daemon. If a command fails with `permission denied`, the
sandbox may be blocking its loopback port or Unix socket. Retry the same command
with `dangerouslyDisableSandbox: true` when supported and authorized.
Do not start or restart the daemon because a sandboxed status probe cannot reach
it.

## Look first

Before your first message, inspect what already exists. These checks are
read-only and need no approval:

```bash
roborev doctor --from-skill --json
roborev config --from-skill validate
roborev config --from-skill list --show-origin
roborev config --from-skill get sync.enabled
git config --show-origin --get core.hooksPath
```

Doctor reports which agents are installed, which agent runs reviews here, and
whether the Git hooks are installed. `core.hooksPath` shows whether the
repository's hooks come from somewhere else. Also read `.roborev.toml`,
`REVIEW.md`, existing rubric files, `.kata.toml`, and agent instruction files
such as CLAUDE.md. A `.kata.toml` means the repository tracks its work in Kata.
List the layout, file types, headings, and recent commit subjects. Reading the
material itself, or comparing drafts against sources, is recon that the user
approves first.

Use what you find to skip questions you can answer. Do not recite it.

## What do you already know?

- **Everything that matters is in the request**, such as "check every quote in
    `drafts/` against the exhibit text in `exhibits/`, on each commit". Skip the
    conversation and go to [Propose and get approval](#propose-and-get-approval).
- **A change to an existing review**, such as one rubric or the batch size.
    Ask only what the change needs, describe it in chat, and get approval.
- **A new review with open questions.** Start the conversation.

## Your first message

Your first message has two parts:

1. One line saying that if they would rather skip the questions, you can propose
    a setup from what you found. If they take that offer, state your defaults in
    plain words and go to the proposal. They still approve it before you write.
1. One open question that gets them describing a concrete moment, such as "What
    is in these drafts, and what is the mistake you most want caught before one
    goes out?"

That's the whole message.

## The conversation

End each message with one question, without a list of example answers. Use the
user's words. Say "reviewer", "check", and "source documents"; avoid
"subagent", "panel", and "custom review type" unless the user uses them first.

Get them describing before you offer options. Offer a short menu only when they
are stuck. When they say "just guess", propose something concrete and say it is
your guess. A guess they can correct ("I'm guessing the timeline file is the
source of truth for dates?") often beats a question.

Ground to cover. Treat this as territory, not a script:

- **The material:** what it is, who relies on it, and what happens when it is
    wrong.
- **The checks:** what a careful human reviewer would check.
- **Sources of truth:** what each check compares against, such as files in the
    repository, scans or PDFs, or an outside database. Reviewers read only the
    repository and their prompt; see [the rules](#turn-answers-into-configuration).
- **What to leave alone:** findings they would reject on sight, house
    conventions, and known non-issues.
- **What fails:** which mistakes must block and which are worth noting. Ask
    with their own example: "Would a misspelled party name fail the review, or
    just be noted?"
- **When it runs:** every commit, every few commits, on request, or on pull
    requests.
- **Who acts on findings:** the user, a writing agent, or someone else, and
    whether findings should also land in Kata when the repository uses it.
- **Where the material may go:** see [Confidential material](#confidential-material).
- **Which agent runs it:** the agent and model they already use and trust. Find
    out which choices they want to make and which they hand to you.

Questions that work anchor in specifics: "Walk me through the last draft that
went out with a mistake." "If the reviewer could catch only one kind of mistake,
which one?" "What finding would make you roll your eyes?"

Once you have some grounding, offer recon: reading a few drafts and sources to
propose concrete checks. Say what you would look for. They decide.

Before proposing, consider several shapes privately: one reviewer with a
combined checklist, or several reviewers. Each reviewer is one more agent call.
Split checks only when they need different sources, a different agent, or a
different focus that one rubric would blur. Drop ideas that fight what the user
told you.

## Play it back

As soon as you understand one part, describe it back in about 150 to 250 words,
then return to questions. Parts are usually:

1. What the review is for and what it protects.
1. Each reviewer: what it checks, against what, what it ignores, and what fails.
1. When it runs and what happens to the findings.

Mark your guesses as guesses; only what they said goes in as theirs. End each
chunk by asking what is wrong or missing.

## Turn answers into configuration

This mapping is for you, not the user. Use only what the conversation needs.

| They want | Configure |
|-----------|-----------|
| One kind of check | One `[review.types.<name>]` with a rubric `template` |
| Several checks | One type per check, one `[review.subagents.<name>]` per type, and a `[review.panels.<name>]` listing them |
| Context every reviewer needs | `review_guidelines` in `.roborev.toml`, or `REVIEW.md` |
| A source reviewers compare against | Commit its text and name the path in the rubric |
| A short source every review needs in full | `[review.types.<name>.includes]`, rendered with `{{ index .Includes "<key>" }}` |
| One custom check after every commit or every N commits | A type, a subagent that uses it, and a one-member panel; set `hook_review_panel` under `[review]`. Automatic reviews use this panel and carry no review type. Set `post_commit_batch_size = N` for every N commits. |
| Several custom checks after every commit or every N commits | A type and subagent per check, a panel listing those subagents, and `hook_review_panel` under `[review]`. Set `post_commit_batch_size = N` for every N commits. |
| Keep the usual code review after commits too | Add a subagent with `review_type = "default"` to the hook panel |
| The whole branch each time | `post_commit_review = "branch"` |
| Reviews on request | `[review] default_panel`, or `--type` and `--panel` |
| Reviews on pull requests | `[ci] panel`; see <https://roborev.io/docs/advanced/subagent-review-panels/#ci-panels> |
| Only serious findings fail | `review_min_severity`, plus severity guidance in each rubric |
| Passing reviews close themselves | `auto_close_passing_reviews = true` |
| Reviewers that see the task a commit is for, from Kata | `[kata_context]` with `mode = "current"`; commits name their issue, such as `Closes: kata#abc4` |
| Failed reviews and findings filed as Kata issues | A `[[hooks]]` entry with `event = "review.*"` and `type = "kata"` |
| Reviewers that read changed PDFs, scans, or images | `binary_evidence = true` on the type; see <https://roborev.io/docs/advanced/custom-review-types/#binary-evidence> |
| A reviewer that looks things up through an outside service | A strict `tools` policy on the type that grants exact MCP tools; see <https://roborev.io/docs/advanced/custom-review-types/#granting-mcp-tools> |
| Files reviewers should skip | `exclude_patterns` |
| Reviewers that skip the setup itself, for non-code material | `exclude_patterns = [".roborev.toml", "review-rubrics/"]` |
| A particular agent or model | `agent`, `model`, and `reasoning` in the repository, type, or subagent |
| A reviewer on trial | `non_voting = true` on the subagent |
| A reviewer that may flake | `allow_failure = true` and `timeout` on the subagent |

Rules that matter:

- Put the setup in the repository's `.roborev.toml`. Use the global
    `~/.roborev/config.toml` only when the user wants the same review in every
    repository, and only with their approval.
- Custom types need an agent with JSON Schema output: `codex`, `claude-code`,
    `pi`, or `grok`. Pi also needs its `pi-json-schema` extension, which
    validation and doctor do not check. Custom types do not inherit
    `review_agent`. Their agent resolves from type-specific repository settings,
    the repository's generic `agent`, type-specific global settings, then global
    `default_agent` (or `codex` when none is set). Check the effective agent for
    each custom reviewer before leaving `agent` unset. If it differs from the
    reviewer the user wants, ask and set the supported agent on the type or
    subagent. When the user restricts providers, see
    [Confidential material](#confidential-material). Otherwise ask which
    supported agent to use. Do not choose one for them.
- `review_min_severity` applies to every reviewer. Align each rubric's
    severity levels so what the user said must fail sits at or above that one
    threshold. Findings below it still appear in the review; they just do not
    fail it.
- `hook_review_panel` replaces whatever ran after each commit before,
    including the usual code review and any global `hook_review_panel`. For
    code, ask whether the usual review should keep running, and if so add a
    `default` member to the panel.
- Type names start with a lowercase letter, use lowercase letters, digits,
    and hyphens, and are at most 64 characters. These names are reserved:
    `default`, `review`, `general`, `security`, `design`, `lookahead`,
    `test-value`, `goal`, `fix`, `refine`, and `classify`.
- Keep rubrics and sources outside the snapshot directory (`.roborev/` by
    default). It is ignored by Git, and `roborev init` refuses to run while
    tracked files sit under it. Use a committed directory such as
    `review-rubrics/`.
- Reviewers see the diff, the rubric, and the guidelines, and read other files
    in the checkout with read-only tools. They cannot fetch URLs. Commit the
    text a check needs. If a source can be neither committed nor reached
    through an MCP grant, tell the user the reviewer can check only what the
    repository holds.
- For PDFs, scans, and images, set `binary_evidence = true` on the type.
    Reviews of a commit or range then get a saved copy of each binary file
    those commits add or change. Files the commits do not touch are not
    included, so for a source that stays unchanged, such as an exhibit that
    later drafts cite, also commit an exported text copy beside it. Reviews of
    uncommitted changes fail when the type sets `binary_evidence`.
    `exclude_patterns` removes files from the evidence as well as the diff, so
    never exclude binary files a reviewer must read.
- The reviewer opens each saved copy with its own tools, so match the agent to
    the files. Claude Code and Grok read PDFs and images directly. Codex reads
    images, but reads a PDF only by running a converter such as `pdftotext` on
    the machine that runs the review, and gets no text from a scanned PDF. For
    PDF evidence, propose `claude-code` or `grok` on the type, or check that the
    converter is installed. Before relying on Pi, have the user confirm it can
    read the file types involved.
- A reviewer reaches an outside service only through exact MCP grants on a
    strict type: a `[review.types.<name>.tools]` table with
    `mode = "read-only"` and an `mcp` table that names each server and its
    tools. Strict types run only on Claude Code, so set `agent = "claude-code"`
    on the type. A backup agent that cannot enforce the policy fails the review
    instead of running it. The global `~/.roborev/config.toml` must define the
    server in `review_mcp_config` and list each `server/tool` pair in
    `review_mcp_allowed_tools`; repository config cannot set either. Propose
    those global lines separately and write them only with the user's
    approval. See
    <https://roborev.io/docs/configuration/#strict-review-tool-policies>.
- Includes are read when the review is built and count toward
    `max_prompt_size`, so include only short sources. Name long sources, and
    sources that will grow, by path.
- When the material is not code, say so in `review_guidelines` so every
    reviewer reads it that way, and in each rubric so trial reviews read it
    that way before the guidelines take effect.
- `fix` and `refine` are writable, code-framed workflows. For non-code
    material, the user or a writing agent corrects findings. Say so before the
    user turns on the Agent Hook, which hands findings to `/roborev-fix`.
- Local reviews read custom types, panels, hook settings, and rubrics from the
    checkout. Review guidelines and `REVIEW.md` come from the default branch,
    which is `origin/<default>` when the repository has an `origin` remote, so
    they apply only after the setup is pushed there. CI reads everything from
    its trusted base ref. Commit the whole setup to the default branch so all
    three agree, and tell the user the guidelines wait on that push.
- A `type = "kata"` hook files an issue for each reviewer's failing review as
    well as for the combined review. With a panel of several reviewers, one
    failing commit can open several issues. Say so when proposing it.

Example: a legal brief checked every five commits by two reviewers. It leaves
the agent to the repository or machine default. Adapt it; do not copy names
that do not fit.

```toml
# .roborev.toml
post_commit_batch_size = 5
review_min_severity = "medium"
review_guidelines = """
This repository holds drafts of a legal brief, not software. Review the prose
and its sources. Do not comment on Markdown formatting or file layout.
"""

[review]
hook_review_panel = "brief"

[review.types.facts]
template = "review-rubrics/facts.md"

[review.types.facts.includes]
timeline = "evidence/timeline.md"

[review.types.quotes]
template = "review-rubrics/quotes.md"

[review.subagents.facts]
review_type = "facts"

[review.subagents.quotes]
review_type = "quotes"

[review.panels.brief]
members = ["facts", "quotes"]
```

## Write the rubrics

A rubric is a Go template, usually 20 to 60 lines. Write it from the user's own
words:

- What the material is and who relies on it, in one or two sentences.
- The checks, as a concrete list.
- What to check against: named files or included text.
- What not to flag.
- Severity in the material's terms, with an example from their work for each
    level.
- Where to point: the file and line, or the section, of the problem.
- When a source is missing, report a finding that names it. Use the
    `unable_to_review` verdict only when the change cannot be assessed at all;
    roborev treats it as an agent failure and retries.
- Report every real finding. The configured threshold decides pass or fail.

roborev appends the review target, the diff, and the output format after the
rubric, so do not add diff placeholders. Only `.ReviewType` and `.Includes` are
available. Write a literal `{{` as `{{"{{"}}`.

```markdown
# Fact check

You are checking a draft legal brief that will be filed with a court. A wrong
date or a misstated event undermines the brief with the court.

The timeline below is the source of truth for dates and events:

{{ index .Includes "timeline" }}

In the changed text, check that:

- every date matches the timeline;
- every event the timeline covers is described the way it records it;
- every fact the argument relies on appears in the timeline or a cited exhibit.

Each exhibit in `exhibits/` has a text copy beside it. Read the text copy when
a fact cites an exhibit.

Do not flag citation format, writing style, or the legal argument.

Severity:

- high: a date or event contradicts the timeline or the cited exhibit.
- medium: a fact the argument relies on has no support in the record.
- low: an imprecise detail that would not mislead a reader.

Give each location as the file and line of the sentence in the draft.
```

## Confidential material

Before proposing, tell the user where the material can go:

- the provider behind each reviewer and synthesis agent, and the account or
    endpoint each agent CLI is signed in to; roborev does not choose it;
- any other agent a review can reach: the repository or machine default agent
    for reviews outside the panel, and configured backup agents;
- every agent priced in `budget.agent_costs`, when `budget.enabled` is true;
    near the daily cap, budget routing can move a review that is not part of
    a panel to any of them, even when its type names an agent;
- the design-review classifier (`classify_agent`, `claude-code` by default)
    and the design reviewer's agent, when `auto_design_review.enabled` or
    `auto_design_review.hook_enabled` is true; a panel run with no design
    member can start both, whatever agents its members pin;
- the MCP servers a strict review type is granted, which receive whatever
    the reviewer sends in its tool calls;
- roborev's local database, which stores prompts and reviews;
- the PostgreSQL mirror, when `sync.enabled` is true;
- pull request comments, when CI reviews cover this repository;
- the Kata project, when a `type = "kata"` hook files findings there, and
    Kata issue text, which `[kata_context]` adds to review prompts.

Ask whether that is acceptable. Do not decide for them. If the user allows only
some providers, every job that can carry the material must name an approved
agent itself. An agent inherited from a default can come from `review_agent`
or another workflow setting, and can fall back to any installed agent.

- Set `agent` on every subagent in every panel, built-in members such as a
    `default` reviewer included, and on every custom type.
- Set `synthesis_agent` on every panel, and the repository `agent` and
    `default_panel`.
- Check `roborev config --from-skill list --show-origin` for `backup_agent`
    and `*_backup_agent`, and every panel for `synthesis_backup_agent`. Each
    backup must be an approved agent or be removed.
- Check `budget.enabled` and `budget.agent_costs`. Budget routing does not
    change panel members or synthesis, but can move any other review. While it
    is on, run every review through a panel, a one-member panel for a single
    type, or get approval to change the budget policy.
- When either `auto_design_review` setting is on, propose `enabled = false`
    and `hook_enabled = false` under `[auto_design_review]` in `.roborev.toml`.
    The design review it starts is a standalone job, so budget routing can
    move it too.
- In the proposal, name the agent each reviewer, combining call, and backup
    will use.

Propose turning off automatic design review for non-code material even
without a provider restriction; a design review of prose is noise.

## Propose and get approval

Send one proposal:

1. A short summary of what the review is for.
1. Each file you will create or change, with one plain line on what it does.
1. Changes outside the repository, such as global configuration, listed
    separately.
1. Your calls: each default you chose that the user did not state, one line
    each.
1. How it will run: when reviews start, which agents run, and how many agent
    calls one review uses. A panel uses one call per reviewer, plus one
    combining call when two or more reviewers finish and any of them reports
    a finding or does not pass.
1. The commit you will make after the checks pass: branch and message.
1. The full content of every file: `.roborev.toml` (or the exact change to an
    existing one) and each rubric.

<HARD-GATE>
Write nothing until the user approves the proposal. Approval covers only what
you showed, including the commit it named. A later change to what a reviewer
checks, what fails, or where the material goes comes back for approval. If they
opted out of the questions, they still approve the proposal before you write.
</HARD-GATE>

## Build and verify

After approval:

1. Write the approved repository files. Edit only the keys you proposed in an
    existing `.roborev.toml`.

1. Write global settings only if the user approved them.

1. Check the result:

    ```bash
    roborev config --from-skill validate
    roborev doctor --from-skill --json
    git check-ignore --no-index -v review-rubrics/facts.md
    ```

    Validation and doctor must pass, but they do not open rubrics, includes, or
    the sources a rubric names. Check those yourself:

    - `git check-ignore` prints nothing, and exits 1, for each rubric, include,
        and source file.
    - Each template, include, and named source exists and is committed by the
        time you finish.
    - Each template's only `{{` are intended template actions.
    - Use doctor to confirm the ordinary-review agent and any default or hook
        panels it reports. Doctor does not report standalone custom types or
        manually selected panels. For those, resolve each member's effective
        agent from its type and subagent settings and the fallback chain above;
        confirm that agent supports custom types.

1. Make the commit named in the proposal, using the repository's commit
    conventions.

1. Ask about the two remaining steps one at a time, and run each only if the
    user says yes:

    - **Hooks**, if automatic reviews are wanted and doctor reports them
        missing. `roborev init --from-skill` installs Git hooks that start
        reviews after commits, starts roborev's background service, and adds
        its snapshot directory to `.gitignore`. If `core.hooksPath` comes from
        global Git configuration, do not run it; it would install into that
        shared directory. Tell the user reviews run only on request until those
        hooks call roborev, and list that as pending.
    - **A trial review** (below).

## Trial review and tuning

A trial review is the fastest way to learn whether a rubric works. It spends
agent calls and sends material to the agents, so ask first. Choose a commit or
range that changes the material and contains every named source. With
`isolate_reviews = true`, the daemon checks out the reviewed commit or range
head, so a source added after the material commit is unavailable when reviewing
only that older commit. Verify each source at the chosen target with
`git show <target>:<path>`. If needed, review a range that includes the material
change and ends at a commit containing the sources, such as the setup commit.
Hooks review only commits made after setup, so material committed earlier is
reviewed only when someone asks.
Offer one review of the commit or range that holds it. Use `--panel none` for
a standalone custom type so a configured `default_panel` cannot replace the
rubric with its members. When providers are restricted and budget routing is
on, use a panel instead; see [Confidential material](#confidential-material).

```bash
roborev review --from-skill --type facts --panel none --wait
roborev review --from-skill --panel brief --wait
```

Without a commit, these review `HEAD`. To review another commit or a range,
verify each ref with `git rev-parse --verify` first and pass it as a quoted
argument, or use `--since <commit>`. For a panel, the job printed at enqueue is the combined
review. Read a member's own review with:

```bash
roborev show --from-skill --job <member-job-id>
```

Show the user the verdict and findings, then tune together:

- A finding they would reject becomes a "do not flag" line in that rubric.
- A mistake the reviewer missed becomes a check.
- A severity they disagree with changes the severity guidance.

Show each rubric change, get approval, commit it, and request a new review.
Rerunning an old job reuses its frozen settings.

## Hand off

Finish with a short summary:

- Files created or changed, and whether they are committed on the default
    branch.
- How reviews run: when they start, which reviewers, and which agents.
- How to run one on request, and how to read results: `roborev tui`,
    `roborev list`, or `roborev show --job <id>`.
- How to tune: edit a rubric, commit, and review again.
- How to add a source document: commit its text where the rubric expects it,
    and update the rubric or include when the path changes.
- What is still pending, such as hooks, a stopped daemon, guidelines waiting
    on a push to the default branch, or the confidentiality decision.

## Red flags

| Thought | Reality |
|---------|---------|
| "I'll write a standard config and skip the questions" | The rubric is the product. Find out what they need caught. |
| "Options will save them effort" | Options steer. Ask them to describe first. |
| "I'll pick the agent I know best" | Keep the agent they run, or ask among the agents that support custom types. |
| "The reviewer can look that up online" | Reviewers read only the repository and their prompt. Commit the source text. |
| "Rubrics can live under `.roborev/`" | That directory is ignored and blocks `roborev init`. |
| "`roborev fix` can apply the findings" | Fix and refine are code-framed. Prose is corrected by the user or a writing agent. |
| "They said it sounds good" | Approval covers what you showed. Show the files before writing them. |
| "The source is missing, so return `unable_to_review`" | That counts as an agent failure. Report a finding that names the missing source. |
