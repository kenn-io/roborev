---
title: GitLab Integration
description: Review GitLab merge requests from a CI pipeline and post results as MR notes
---

roborev can review GitLab merge requests from inside a GitLab CI job and post
the synthesized result as a merge request note. This is the same daemon-free
"pipeline mode" the GitHub integration offers through
[`roborev ci review`](/commands/#ci-review): the review matrix runs in the CI
job, results are synthesized into one comment, and nothing is stored between
runs.

!!! note

    The daemon CI poller (`[ci]` config section) is GitHub-only today. GitLab
    support for the poller is coming separately; until then, use the pipeline mode
    described on this page.

## How It Works

On each merge request pipeline, the job:

1. Checks out the merge request branch with full history
1. Auto-detects the review range as `<base>..<head>` from
    `CI_MERGE_REQUEST_DIFF_BASE_SHA` and the merge request head
    (`CI_MERGE_REQUEST_SOURCE_BRANCH_SHA` when set, otherwise `CI_COMMIT_SHA`)
1. Runs the configured `review_type x agent` matrix in parallel
1. Synthesizes the results into one comment
1. Posts (or updates) a merge request note

Everything except the note posting is forge-neutral, so the `.roborev.toml`
`[ci]` settings behave exactly as they do on GitHub.

## Required Token Setup

`--comment` needs an API token that is allowed to create merge request notes.

!!! warning

    The pipeline's built-in `CI_JOB_TOKEN` **cannot** create merge request notes. A
    separate token is required.

Create a **project access token** (Settings > Access tokens) scoped to the
single project being reviewed:

| Setting | Value |
|---------|-------|
| Scope | `api` (narrowest scope that can create notes) |
| Role | `Reporter` (do not grant more) |

Then expose it to the pipeline as a CI/CD variable named `GITLAB_TOKEN`
(Settings > CI/CD > Variables), marked **Masked** and **Protected**. Read
[Trust Model](#trust-model) before turning **Protected** off — which is what a
merge-request-branch pipeline needs in order to see the variable at all.

roborev resolves the token in this order:

1. `GITLAB_TOKEN`
1. `GL_TOKEN`
1. `glab auth token` (the [glab](https://gitlab.com/gitlab-org/cli) CLI, useful
    for local runs)

Group access tokens and personal access tokens are also accepted, but they carry
access far beyond the reviewed project, so prefer a project access token.

### Trust Model

Anyone who can run a pipeline on a branch can read every CI/CD variable exposed
to that pipeline. Job scripts and `.gitlab-ci.yml` come from the branch under
review, so an author can change them to print, upload, or otherwise exfiltrate
`GITLAB_TOKEN`. **Masked** only redacts the value from job logs — it does not
stop job code from reading it. There is no configuration that hands a token to
an untrusted branch pipeline safely.

One consequence is worth stating on its own, because it also applies to the
protected setup below. Without `--gl-host`, roborev resolves the GitLab API
origin from `CI_SERVER_URL`, falling back to `GITLAB_HOST` and `GL_HOST`, and
sends the token to whatever that resolves to. Predefined variables sit at the
bottom of GitLab's
[variable precedence](https://docs.gitlab.com/ci/variables/#cicd-variable-precedence),
so a pipeline started with custom variables can override `CI_SERVER_URL` and
point the token at another host. Starting a protected-branch pipeline with
variables does not require the ability to change any code: push or merge rights
on the branch suffice, and so does a
[pipeline trigger token](https://docs.gitlab.com/ci/triggers/) — which can
otherwise only start pipelines, so redirecting the origin would escalate it into
the API token itself. A protected CI/CD variable is no defense either: pipeline
variables outrank project variables too.

Pin the origin whenever `GITLAB_TOKEN` is worth protecting: pass
`--gl-host https://gitlab.example.com` in the job script, hardcoded. The script
comes from the `.gitlab-ci.yml` of the ref the pipeline runs on — for a
protected-branch pipeline, the protected ref — so pipeline variables cannot
rewrite it. Do not expand a variable there (`--gl-host $CI_SERVER_URL` hands the
choice right back to whoever started the pipeline). The flag also pins the forge
choice itself: it selects GitLab outright, so an injected `GITHUB_ACTIONS=true`
cannot steer the run to the GitHub client — which matters for jobs that carry a
GitHub token for the `copilot` or `kiro` agents, since that client resolves its
origin from the equally injectable `GITHUB_API_URL`. A pinned run also stops
consulting `HTTP_PROXY`/`HTTPS_PROXY`: a proxy redirects where the token's bytes
go without changing the hostname, and those are ordinary variables a pipeline
starter can set. The trade-off is that a pinned job cannot egress through a
proxy; drop the pin if your runner requires one, and treat that as choosing the
unprotected setup below. Independently of the pin, roborev refuses to send the
token to a plaintext `http` origin (loopback excepted) or to a URL that embeds
credentials, whichever way the origin was resolved.

One environment-controlled knob is deliberately not neutralized: the system
certificate store still honors `SSL_CERT_FILE` and `SSL_CERT_DIR`, because
self-hosted instances behind an internal CA need them. On its own a substituted
CA grants nothing — it lets the job trust a certificate, not intercept a
connection — but combined with a network position it would. Pinning the origin
removes the proxy half of that pair, which is the half a pipeline variable can
supply.

The pin closes the redirect channel; it does not change who may hold the token.
Anyone who can run job code on a branch whose pipeline can read `GITLAB_TOKEN`
can still exfiltrate it directly, which is what the choice between the two
setups below is about.

That leads to two setups, and they are not interchangeable — pick one before you
copy the example job below.

- **Protected variable (recommended).** Keep `GITLAB_TOKEN` protected so only
    pipelines on protected branches can read it. Merge-request-branch jobs then
    cannot reach the token, so the note has to be posted from a pipeline running
    on a protected or default branch. GitLab sets no `CI_MERGE_REQUEST_*`
    variables there, so that job must supply the merge request itself:
    `roborev ci review --comment --pr <iid> --ref <base>..<head> --gl-host https://gitlab.example.com`,
    driven by whatever already knows the IID (a
    [pipeline trigger](https://docs.gitlab.com/ci/triggers/) or a
    [webhook](https://docs.gitlab.com/user/project/integrations/webhooks/)
    listener that passes it in). There is no auto-detection for this path. The
    merge request head is not part of a default-branch checkout, so the job must
    fetch it first: `git fetch origin refs/merge-requests/<iid>/head`.

    Fetching makes the commits reachable, but it does not move the working tree:
    the job is still checked out on the protected branch. The `<base>..<head>`
    range is correct, so the diff under review is right — but agents that open
    files for surrounding context read the protected branch's copy of them, not
    the merge request's, which makes that context stale and can mislead a
    finding. Check the head out and review from there if that matters:

    ```bash
    git fetch origin refs/merge-requests/<iid>/head
    mr_tree="$(mktemp -d)"
    git worktree add "$mr_tree" FETCH_HEAD
    cd "$mr_tree"
    PATH=/usr/local/bin:/usr/bin:/bin \
      /usr/local/bin/roborev ci review --comment --pr <iid> \
      --ref <base>..<head> \
      --gl-host https://gitlab.example.com \
      --gl-repo mygroup/myproject \
      --upsert-comments=false \
      --agent claude-code --review-types security,default --min-severity medium
    ```

    `--pr` and `--ref` arrive as separate inputs here, and whoever starts the
    pipeline chooses them, so roborev binds them itself. With an explicit `--pr`
    it asks the API for the merge request's head and base, and requires the
    reviewed range to be a real `BASE..HEAD` range that ends at that head and
    starts no later than that base. Without the full check, a trigger could
    review a harmless range — or a narrowed one like `HEAD~1..HEAD`, which ends
    at the right commit while hiding every earlier one — and have the bot post a
    passing verdict on code nobody read, or, with `upsert_comments`, replace a
    note that carried findings. Starting earlier than the merge request's base
    is allowed, since reviewing more omits nothing. The check runs before the
    review matrix and again just before posting, so a force push landing
    mid-review is caught too. Derive the range from the merge request
    (`git rev-parse FETCH_HEAD` after the fetch above, and `git merge-base`
    against the target branch) rather than from trigger-supplied variables, and
    it will match.

    The range must start exactly where the merge request's diff does. Starting
    earlier is refused as well as starting later: reviewing more looks harmless,
    but the diff is computed tree to tree, so a change the merge request makes
    can cancel against an opposing change between the two bases and vanish from
    what the agents see.

    One thing roborev cannot defend here, because it happens before roborev runs:
    a loader variable such as `LD_PRELOAD` in the job environment is applied by
    the dynamic loader when the `roborev` process itself starts, and can point
    at a file committed to the merge request. roborev strips those variables
    from the processes it spawns, but by then its own process is already running
    with them. Unset `LD_PRELOAD`, `LD_AUDIT`, `DYLD_INSERT_LIBRARIES`, and
    `NODE_OPTIONS` in the protected job before invoking roborev if pipeline
    variables there are not trusted.

    Ranges roborev derives from the CI variables are checked the same way, because
    those variables are themselves overridable by whoever starts the pipeline;
    trusting them would leave open the hole the flags close. The practical
    consequence is for merged results pipelines on a shallow clone, where the
    checked-out commit is GitLab's synthetic merge commit rather than the merge
    request head: the job now stops and asks for more history instead of
    reviewing that commit, so keep `GIT_DEPTH: 0` as the example does. A head
    the merge request has already moved past is reported as a race rather than
    an error — the job says the head moved and exits without posting, leaving
    the verdict to the pipeline for the new head.

    That comes with a trade-off, and it is the reason the flags above are not
    optional: from inside the worktree, roborev reads `.roborev.toml` from the
    merge request's tree, so its author controls the `[ci]` settings. Left
    unpinned, they could swap in a weaker agent, drop review types, or raise
    `min_severity` to hide their own findings. Flags outrank the file, so pin
    whatever must not be author-controlled. Model settings are the gap worth
    knowing about: `ci review` has no `--model` flag, and a model spec may carry
    a proxy URL (`<model>@https://host`), so an author who controls
    `.roborev.toml` can route the review through a server that returns whatever
    verdict they like. If that matters, review from the protected checkout
    rather than the merge request worktree, which is the default described
    above. `--min-severity` pins both halves of the run: the synthesis filter
    and the threshold the individual reviews are prompted with, so a
    `review_min_severity` in the merge request's tree cannot stop findings from
    being reported in the first place. `review_guidelines` has no flag: it is
    read from the default branch, but only when that branch both resolves in the
    checkout *and* has a committed `.roborev.toml`. If either is missing — and a
    project that never committed one always misses the second — it falls back to
    the merge request's tree, where the author controls it. Commit a
    `.roborev.toml` on the default branch and make sure the job can resolve that
    branch (fetch it, or set `origin/HEAD`). The token stays out of the agents
    either way — this is about the review's integrity, not its exposure. If the
    runner reuses its workspace, finish with
    `cd "$CI_PROJECT_DIR" && git worktree remove --force "$mr_tree"` so the next
    run starts clean; the `cd` matters because git refuses to remove the
    worktree you are standing in.

    A merge request pipeline (the unprotected-variable setup) has no such gap: the
    runner already checks out the merge request branch — or, in a merged results
    pipeline, the simulated merge of it — so the files agents read already
    contain the change.

- **Unprotected variable.** Only appropriate when every author who can open a
    merge request is already trusted with the token's access — for example an
    internal project with no forks and no external contributions. This is the
    setup the merge request pipeline example below needs. Do not use it on
    public projects or anywhere merge requests arrive from forks.

Either way, keep the token's blast radius small: one project, `api` scope,
`Reporter` role, and rotate it on a schedule.

The review agents get no token in their environment — though the checkout is a
separate matter, covered below. Their input is the merge request under review,
so it is untrusted, and an agent that follows an injected instruction could copy
any value it can read into the review text — which is then posted as a note.
roborev therefore removes `GITLAB_TOKEN`, `GL_TOKEN`, and `CI_JOB_TOKEN` from
every agent subprocess environment, keeping them only in its own process, where
the note is posted from. The aliases that carry the same job credential under
another name go with them — `CI_REGISTRY_PASSWORD`, `CI_REPOSITORY_URL` (which
embeds the token in a clone URL), `CI_DEPLOY_PASSWORD`,
`CI_DEPENDENCY_PROXY_PASSWORD`, the deprecated `CI_JOB_JWT*` tokens, and the
pre-9.0 `CI_BUILD_TOKEN` and `CI_BUILD_REPO` spellings GitLab still injects —
since stripping only `CI_JOB_TOKEN` would leave it readable from the
environment. The same scrub covers the `--help` capability probes roborev runs
before a review, which would otherwise start the agent binary with the tokens
still in place, and it drops the runtime preload variables — `NODE_OPTIONS`,
`LD_PRELOAD`, `LD_AUDIT`, `DYLD_INSERT_LIBRARIES` — that would run code of the
pipeline starter's choosing inside those processes. Set `NODE_OPTIONS` for your
own job steps if you need it, not for the review.

This bounds the obvious channels; it is not a sandbox. Agents run as the same
user as roborev, so on Linux one that executes commands can still read the
parent's environment through `/proc`, and roborev holds the token there because
that is where the note is posted from. Treat the strip as removing the casual
path, and the choice of setup below — not the strip — as what decides whether an
untrusted author's content is reviewed by a job holding a token worth stealing.
Non-secret identity such as `CI_SERVER_URL`, `CI_PROJECT_PATH`, and
`CI_REGISTRY_USER` stays. `GH_TOKEN` and `GITHUB_TOKEN` are removed too, except
for the `copilot` and `kiro` CLIs, which authenticate with a GitHub token and
cannot run without it; agents launched over ACP have no such exemption, and
roborev logs the variables it removed so a resulting auth failure is
diagnosable.

Clearing the environment is not the whole story: a default GitLab Runner clones
from a credentialed URL and leaves it in the checkout's `.git/config` as
`origin`, so `git remote -v` still shows the job token. roborev does not rewrite
the remote. If that matters for your setup, run the job with the runner's
`FF_GIT_URLS_WITHOUT_TOKENS` feature flag or strip the credentials from the
remote before the review runs. Either way this only bounds what a
prompt-injected agent can reach; it does nothing about the job script itself,
which is why the choice between the two setups above still decides the trust
model.

Review text is model-generated and the merge request under review is part of its
input, so the note body itself is untrusted. GitLab runs
[quick actions](https://docs.gitlab.com/user/project/quick_actions/) —
`/approve`, `/close`, `/target_branch` — from any note line that starts with a
slash, using the note author's permissions. Before posting, roborev escapes
every line whose first character is a slash followed by a letter, so such a line
renders as text instead of running. That is why the occasional `\/` shows up
inside a posted code sample.

## Example `.gitlab-ci.yml`

```yaml
roborev:
  stage: test
  image: node:22-bookworm
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
  variables:
    GIT_DEPTH: 0 # full history, needed for the base..head range
  before_script:
    - npm install -g @anthropic-ai/claude-code
    - curl -fsSL https://roborev.io/install.sh | bash
    - export PATH="/usr/local/bin:$HOME/.local/bin:$PATH"
  script:
    - roborev ci review --comment --agent claude-code
```

Any image works as long as the job can reach three things: `git` (roborev shells
out to it for the `base..head` diff), the runtime the chosen agent CLI needs,
and that agent CLI itself on `PATH`. `node:22-bookworm` is a convenient base
because it already ships git, curl, and Node — the Claude Code CLI
(`@anthropic-ai/claude-code`) needs Node 18 or newer. Selecting a different
agent means installing that agent's CLI instead.

Inside a merge request pipeline, `ci review` reads
`CI_MERGE_REQUEST_PROJECT_PATH` (falling back to `CI_PROJECT_PATH`),
`CI_MERGE_REQUEST_IID`, `CI_MERGE_REQUEST_DIFF_BASE_SHA` (with
`CI_MERGE_REQUEST_TARGET_BRANCH_SHA` and `CI_COMMIT_BEFORE_SHA` as fallbacks),
`CI_MERGE_REQUEST_SOURCE_BRANCH_SHA`, `CI_COMMIT_SHA`, and `CI_SERVER_URL`
automatically, so no flags are needed beyond `--comment` and the agent
selection. See [Flag Reference](#flag-reference) for the exact precedence.

Your agent's API key must also be available to the job. Add it as a masked CI/CD
variable using the name the agent expects:

| Agent | Variable |
|-------|----------|
| Claude Code | `ANTHROPIC_API_KEY` |
| Codex | `OPENAI_API_KEY` |
| Gemini | `GOOGLE_API_KEY` |

Codex and Gemini inherit the job environment apart from the forge tokens
described in [Trust Model](#trust-model), so their variables need no further
handling. Claude Code is a special case worth knowing about: roborev strips
inherited `ANTHROPIC_*` variables before launching the CLI so that it, not the
surrounding environment, decides how Claude is routed. `ci review` therefore
reads `ANTHROPIC_API_KEY` from the job itself and passes that credential to the
agent explicitly, which is why the CI/CD variable above works. Setting
`anthropic_api_key` in the global `~/.roborev/config.toml` is the other
supported route, and it takes precedence when both are present.

## Self-Hosted GitLab

No extra configuration is needed inside CI: `CI_SERVER_URL` already points at
your instance, and roborev appends `/api/v4` to it.

Outside CI (for example, running the command from a laptop), set the instance
explicitly:

```bash
export GITLAB_HOST=https://gitlab.example.com
export GITLAB_TOKEN=glpat-...
roborev ci review --gl-repo group/subgroup/project --pr 42 \
  --ref main..my-branch --comment
```

`--ref` is mandatory outside GitLab CI. Ref auto-detection reads
`CI_MERGE_REQUEST_SOURCE_BRANCH_SHA` or `CI_COMMIT_SHA`, neither of which is set
on a laptop, so omitting it fails with
`--ref not provided and auto-detection failed`.

`GITLAB_HOST` accepts either a full URL or a bare hostname. `GL_HOST` is honored
as well, matching the `glab` CLI. `--gl-host https://gitlab.example.com` is the
flag equivalent and wins over all three environment variables. When the token
comes from the `glab` CLI fallback, roborev passes `--hostname` so the correct
instance token is used.

Subgroup paths are supported: `--gl-repo group/subgroup/project`.

## Flag Reference

| Flag | Description |
|------|-------------|
| `--gl-repo <group/project>` | GitLab project path (default: `CI_MERGE_REQUEST_PROJECT_PATH`, then `CI_PROJECT_PATH`); mutually exclusive with `--gh-repo` |
| `--gl-host <url>` | GitLab server URL or hostname; selects GitLab and is mutually exclusive with `--gh-repo` (default: `CI_SERVER_URL`, then `GITLAB_HOST`/`GL_HOST`, then `gitlab.com`). Hardcode it in the job script when `GITLAB_TOKEN` is protected — see [Trust Model](#trust-model) |
| `--pr <iid>` | Merge request IID (default: `CI_MERGE_REQUEST_IID`) |
| `--ref <range>` | Git ref or range to review (default: `CI_MERGE_REQUEST_DIFF_BASE_SHA..<mr head>`) |
| `--comment` | Post the result as a merge request note |
| `--upsert-comments` | Update the previous roborev note instead of adding one (overrides `[ci] upsert_comments`) |
| `--agent <names>` | Agents to use (comma-separated, default: auto-detect) |
| `--review-types <types>` | Review types to run (`security`, `design`, `lookahead`, `default`) |
| `--reasoning <level>` | Reasoning depth (`thorough`/`standard`/`fast`) |
| `--min-severity <level>` | Minimum severity to report (`low`/`medium`/`high`/`critical`) |
| `--synthesis-agent <name>` | Agent for combining multi-job results |

Forge detection precedence, highest first:

1. Explicit flags: `--gl-repo` or `--gl-host` select GitLab, `--gh-repo` selects
    GitHub (combining `--gh-repo` with either GitLab flag is an error)
1. GitLab CI environment (`GITLAB_CI=true`)
1. GitHub Actions environment (`GITHUB_ACTIONS=true`)

The environment order matters when both indicators are present. GitHub Actions
job env comes from the runner or committed workflow code, so `GITLAB_CI` inside
real Actions can only be set deliberately; a GitLab pipeline starter, by
contrast, can inject `GITHUB_ACTIONS=true` as a pipeline variable. The GitLab
indicator is the one that cannot be forged from outside the job script, so it
wins — otherwise an injected variable could steer a GitLab job to the GitHub
client, whose API origin (`GITHUB_API_URL`) and token are read from equally
injectable variables.

When the diff base is unavailable (a branch pipeline rather than a merge request
pipeline), roborev falls back to `CI_MERGE_REQUEST_TARGET_BRANCH_SHA`, then
`CI_COMMIT_BEFORE_SHA`, and finally to the merge base with `CI_DEFAULT_BRANCH`.
That last step covers a branch's first push, where `CI_COMMIT_BEFORE_SHA` is all
zeros: the push usually carries several commits, so reviewing only the tip would
report a verdict on the last one and say nothing about the rest. If the default
branch is unset or missing from the clone, the range cannot be reconstructed and
the job stops and asks for an explicit `--ref` rather than review a fraction of
the push. A branch whose head is already contained in the default branch added
no commits of its own; that commit was reviewed when it landed on the default
branch, so the job prints `no changes to review` and exits zero rather than
reporting on code the push did not touch. The default branch's own first push —
a brand-new repository's initial pipeline — has no base at all: a single-commit
push is reviewed as that commit, and a multi-commit one stops and asks for
`--ref` the same way. The single-commit case is accepted only when the commit is
a real root: a shallow clone that cut a multi-commit push down to its tip still
records the parent in the raw commit object, and roborev checks it and fails
with the same `GIT_DEPTH` guidance instead of reviewing the tip. In practice
`CI_COMMIT_BEFORE_SHA` is the one that runs: GitLab sets
`CI_MERGE_REQUEST_DIFF_BASE_SHA` in every merge request pipeline, while
`CI_MERGE_REQUEST_TARGET_BRANCH_SHA` appears only in merged-results and
merge-train pipelines, so the diff base is always there too and wins first. The
target-branch entry is kept only in case that ever changes.

Both fallbacks are branch tips rather than diff bases, so roborev resolves
`git merge-base <fallback> <head>` in the checkout and starts the range there.
Without that, a target branch that advanced after the branches diverged would
make target-only commits show up as removals, and a force-pushed source branch
would compare against a commit that is no longer an ancestor. If the merge base
cannot be resolved — a shallow clone, or objects that were pruned — roborev logs
a warning and compares the tips directly instead of failing the job. If the base
commit is missing from the clone altogether, the job fails: `base..head` would
hand every later git operation a commit that is not there, and narrowing to the
HEAD commit would review one commit of a larger range and could still post a
passing verdict. Set `GIT_DEPTH: 0` (as the example pipeline does) so the
history the merge base needs is present. `CI_MERGE_REQUEST_DIFF_BASE_SHA` is
used verbatim, because GitLab already resolved it against the target branch.

For the head of the range, roborev prefers `CI_MERGE_REQUEST_SOURCE_BRANCH_SHA`
and falls back to `CI_COMMIT_SHA`. GitLab sets the former only in merged results
pipelines, where `CI_COMMIT_SHA` points at a synthetic merge commit — diffing
that against the diff base would mix target branch changes into the review.

Similarly, the project path is taken from `CI_MERGE_REQUEST_PROJECT_PATH` when
set, because `CI_MERGE_REQUEST_IID` is scoped to the merge request's target
project. `CI_PROJECT_PATH` names the project running the pipeline, which is the
fork in fork merge request pipelines.

## Customizing via `.roborev.toml`

The `[ci]` section is shared with the GitHub integration:

```toml
# .roborev.toml
[ci]
review_types = ["security", "default"]
reasoning = "thorough"
min_severity = "medium"
upsert_comments = true
```

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `review_types` | array | `["security"]` | Review types to run |
| `reasoning` | string | `"thorough"` | Reasoning level |
| `min_severity` | string | `"low"` | Minimum severity to include in output |
| `upsert_comments` | bool | `false` | Update the previous roborev note instead of adding a new one |

With `upsert_comments = true`, roborev finds its previous note on the merge
request (identified by a hidden marker in the comment body) and edits it in
place, so a busy merge request keeps a single, current review note.

## Troubleshooting

**"GitLab authentication required"** — no token was found. Confirm the
`GITLAB_TOKEN` CI/CD variable exists and is visible to this pipeline: a
protected variable is hidden from jobs on unprotected branches, which is
intentional. See [Trust Model](#trust-model) for the options.

**403 when posting the note** — the token's role is below Reporter, or you are
using `CI_JOB_TOKEN`, which cannot create notes.

**Empty or truncated diffs** — set `GIT_DEPTH: 0` (or a depth large enough to
reach the merge base) so the base commit is present in the checkout. GitLab
clones shallowly by default.

## See Also

- [CLI Commands](/commands/#ci-review): Full `ci review` flag reference
- [GitHub Integration](/integrations/github/): The equivalent GitHub setup and
    the daemon CI poller
- [Configuration](/configuration/): Global and per-repo settings
