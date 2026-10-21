#!/usr/bin/env bash
#
# Measures review quality on ReviewBench (https://review-bench.ai) by running
# roborev's review pipeline from one or more git refs on the benchmark's pull
# requests and judging the findings. Each run costs one agent review per pull
# request plus judge calls, so run it only when someone asks for it.
#
#   scripts/reviewbench/eval.sh setup
#   scripts/reviewbench/eval.sh run --ref REF --name NAME [run options]
#   scripts/reviewbench/eval.sh judge --name NAME [judge options]
#   scripts/reviewbench/eval.sh compare NAME NAME...
#   scripts/reviewbench/eval.sh ab --base REF --head REF [run and judge options]
#   scripts/reviewbench/eval.sh harness --ref REF --name NAME --arm A --arm B... [options]
#   scripts/reviewbench/eval.sh panel --ref REF --name NAME --panel PANEL [--baseline RUN] [options]
#
# ab compares two refs with one agent. harness compares agents on one ref:
# each --arm is AGENT:PROVIDER:MODEL, for example codex::gpt-6-sol or
# pi:cpa:gpt-6-sol; PROVIDER only applies to pi and may be empty. panel reviews
# with a roborev panel from --panel-config (default ~/.roborev/config.toml),
# then judges the panel and each member on its own; --baseline names an
# earlier run, such as a stock reviewer, to print beside them.
#
# Run options:
#   --agent NAME        roborev agent (default claude-code)
#   --model MODEL       agent model (default: the agent's own default)
#   --agent-provider P  pi provider for the model
#   --reasoning LEVEL   fast, standard, or thorough (default thorough)
#   --jobs N            reviews run at once (default 4)
#   --set test|full     the 25-PR test set (default) or all 219 PRs
#   --pr INDEX          only this manifest index; repeat for several
# Judge options:
#   --provider NAME     pi provider for the judge (default anthropic)
#   --judge-model ID    judge model (default claude-sonnet-5, the leaderboard's)
#   --concurrency N     judge calls at once (default 4)
#
# The judge reaches models through your pi login, settings, and extensions in
# ~/.pi/agent, the same way the pi command line does.
# Results (findings, scores, logs, reports) live in $ROBOREV_EVAL_DIR, by
# default ~/.local/share/roborev-reviewbench. Benchmark repositories and
# checkouts are disposable and live in $ROBOREV_EVAL_CACHE, by default under
# the system temporary directory.
set -euo pipefail

# ReviewBench is pinned so judged results stay comparable across runs. Its
# judge pins pi 0.75.5, which cannot see providers that pi extensions
# register, so setup moves it to pi 1.x with reviewbench-pi1.patch.
REVIEWBENCH_REPO=https://github.com/review-bench/ReviewBench.git
REVIEWBENCH_SHA=4f04392d6bf4941ab6f8a3ea0c15b449f4458864
PI_VERSION=1.1.0
PI_PACKAGES=(pi-coding-agent pi-ai pi-agent-core pi-codemode pi-mcp pi-tui pi-telemetry chord)

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
probe_src="$root/scripts/reviewbench/probe"
results="${ROBOREV_EVAL_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/roborev-reviewbench}"
cache="${ROBOREV_EVAL_CACHE:-${TMPDIR:-/tmp}/roborev-reviewbench}"
bench="$cache/ReviewBench"

usage() { sed -n '8,41p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
die() { echo "eval: $*" >&2; exit 1; }
log() { echo "eval: $*" >&2; }

agent=claude-code model="" agent_provider="" reasoning=thorough jobs=4 set_name=test prs=() arms=()
panel="" panel_config="$HOME/.roborev/config.toml" baseline=""
provider=anthropic judge_model=claude-sonnet-5 concurrency=4
ref="" name="" base_ref="" head_ref=""

parse_options() {
  while [ $# -gt 0 ]; do
    [ $# -ge 2 ] || usage
    case "$1" in
      --ref) ref="$2" ;;
      --name) name="$2" ;;
      --base) base_ref="$2" ;;
      --head) head_ref="$2" ;;
      --agent) agent="$2" ;;
      --model) model="$2" ;;
      --agent-provider) agent_provider="$2" ;;
      --arm) arms+=("$2") ;;
      --panel) panel="$2" ;;
      --panel-config) panel_config="$2" ;;
      --baseline) baseline="$2" ;;
      --reasoning) reasoning="$2" ;;
      --jobs) jobs="$2" ;;
      --set) set_name="$2" ;;
      --pr) prs+=("$2") ;;
      --provider) provider="$2" ;;
      --judge-model) judge_model="$2" ;;
      --concurrency) concurrency="$2" ;;
      *) usage ;;
    esac
    shift 2
  done
}

setup() {
  if [ ! -d "$bench/.git" ]; then
    git clone -q "$REVIEWBENCH_REPO" "$bench"
  fi
  if [ "$(git -C "$bench" rev-parse HEAD)" != "$REVIEWBENCH_SHA" ]; then
    git -C "$bench" fetch -q origin "$REVIEWBENCH_SHA"
    git -C "$bench" checkout -q --detach "$REVIEWBENCH_SHA"
  fi
  if ! git -C "$bench" apply --reverse --check "$root/scripts/reviewbench/reviewbench-pi1.patch" 2>/dev/null; then
    git -C "$bench" checkout -q -- .
    git -C "$bench" apply "$root/scripts/reviewbench/reviewbench-pi1.patch"
  fi
  local installed=""
  [ -f "$bench/node_modules/@earendil-works/pi-coding-agent/package.json" ] &&
    installed="$(jq -r .version "$bench/node_modules/@earendil-works/pi-coding-agent/package.json")"
  if [ "$installed" != "$PI_VERSION" ]; then
    # Every pi package at one version: caret ranges would pull newer
    # libraries that this pi-coding-agent does not work with.
    local packages=() p
    for p in "${PI_PACKAGES[@]}"; do packages+=("@earendil-works/$p@$PI_VERSION"); done
    (cd "$bench" && npm ci --silent && npm install --no-save --silent "${packages[@]}")
  fi
}

manifest() {
  case "$set_name" in
    test) echo "$bench/corpus/test/test.json" ;;
    full) echo "$bench/corpus/manifest.json" ;;
    *) die "--set must be test or full" ;;
  esac
}

# build_probe REF OUT builds the probe against REF's own review code. The
# probe source comes from this checkout, so refs that predate it work too.
build_probe() {
  local sha tree
  sha="$(git -C "$root" rev-parse --verify "$1^{commit}")" || die "unknown ref $1"
  tree="$(mktemp -d)"
  git -C "$root" worktree add -q --detach "$tree/src" "$sha"
  mkdir -p "$tree/src/scripts/reviewbench/probe"
  find "$probe_src" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cp {} "$tree/src/scripts/reviewbench/probe/" \;
  if ! (cd "$tree/src" && go build -o "$2" ./scripts/reviewbench/probe); then
    git -C "$root" worktree remove --force "$tree/src"
    die "the probe does not build at $1 ($sha)"
  fi
  git -C "$root" worktree remove --force "$tree/src"
  rm -rf "$tree"
  echo "$sha"
}

has_commits() {
  local dir="$1"; shift
  local sha
  for sha in "$@"; do
    GIT_NO_LAZY_FETCH=1 git -C "$dir" cat-file -e "$sha^{commit}" 2>/dev/null || return 1
  done
}

# prepare_pr ENTRY RUN_DIR PR_DIR fetches a pull request into a shared cache and
# checks its head out into a worktree of its own, the way ReviewBench's
# try-agent.sh does: from the review-bench mirror, falling back to upstream.
prepare_pr() {
  local entry="$1" run="$2" prdir="$3" nwo pr base head key repo dir
  nwo=$(jq -r .nwo <<<"$entry"); pr=$(jq -r .pr_number <<<"$entry")
  base=$(jq -r .base <<<"$entry"); head=$(jq -r .head <<<"$entry")
  key="${nwo/\//_}_${pr}-${head:0:8}"
  repo="$cache/repos/${nwo/\//_}"
  dir="$prdir/$key"
  [ -d "$repo/.git" ] || git init -q "$repo"
  git -C "$repo" remote remove origin 2>/dev/null || true
  git -C "$repo" remote remove upstream 2>/dev/null || true
  git -C "$repo" remote add origin "https://github.com/review-bench/${nwo/\//_}.git"
  git -C "$repo" remote add upstream "https://github.com/$nwo.git"
  if ! has_commits "$repo" "$base" "$head"; then
    GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" fetch -q --filter=blob:none origin "$base" "$head" 2>/dev/null || true
  fi
  if ! has_commits "$repo" "$base" "$head"; then
    GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" fetch -q --filter=blob:none upstream "$base" "pull/$pr/head" 2>/dev/null \
      || GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" fetch -q --filter=blob:none upstream "$base" "$head" 2>/dev/null || true
  fi
  has_commits "$repo" "$base" "$head" || { log "cannot fetch $nwo#$pr"; return 1; }
  rm -rf "$dir"
  mkdir -p "$dir"
  git -C "$repo" worktree prune
  GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" worktree add -q --force --detach "$dir/repo" "$head"
  jq '{repo, pr_number: (.pr_number | tonumber), base, head, nwo, title, body}' <<<"$entry" > "$dir/pr.json"
  echo "$key"
}

# review_pr RUN_DIR PR_DIR KEY reviews one prepared pull request, retrying once as
# the benchmark does. Findings that already exist are kept, so a stopped run
# resumes where it left off.
review_pr() {
  local run="$1" prdir="$2" key="$3" attempt
  local out="$run/findings/$key.json"
  [ -s "$out" ] && return 0
  local mode=(-agent "$agent" -model "$model" -provider "$agent_provider" -reasoning "$reasoning")
  if [ -n "$panel" ]; then
    mode=(-panel "$panel" -member-out "$run/member-findings/$key")
  fi
  for attempt in 1 2; do
    if ROBOREV_DATA_DIR="$run/roborev-data" timeout "${PR_TIMEOUT:-1800}" \
      "$run/bin/probe" -repo "$prdir/$key/repo" -pr-json "$prdir/$key/pr.json" \
      "${mode[@]}" -producer "roborev" \
      -out "$out.tmp" -artifacts "$run/artifacts/$key" >>"$run/logs/$key.log" 2>&1; then
      mv "$out.tmp" "$out"
      log "$key: $(jq '.findings | length' "$out") finding(s)"
      return 0
    fi
    log "$key: attempt $attempt failed (see $run/logs/$key.log)"
  done
  return 1
}

run_eval() {
  [ -n "$ref" ] && [ -n "$name" ] || usage
  setup
  local run="$results/runs/$name" prdir="$cache/prs/$name" list sha i count indices key failed=0
  mkdir -p "$run/bin" "$run/findings" "$run/logs" "$run/artifacts" "$run/roborev-data"
  sha="$(build_probe "$ref" "$run/bin/probe")"
  if [ -n "$panel" ]; then
    # Only the review sections: panels, subagents, and review types. Nothing
    # else from the user's roborev config reaches the eval.
    awk '/^\[/{keep = ($0 ~ /^\[review[].]/)} keep' "$panel_config" > "$run/roborev-data/config.toml"
  fi
  list="$(manifest)"
  jq -n --arg ref "$ref" --arg sha "$sha" --arg agent "$agent" --arg model "$model" \
    --arg provider "$agent_provider" --arg reasoning "$reasoning" --arg set "$set_name" \
    --arg bench "$REVIEWBENCH_SHA" \
    --arg panel "$panel" \
    '{ref: $ref, sha: $sha, agent: $agent, provider: $provider, model: $model, reasoning: $reasoning, panel: $panel, set: $set, reviewbench: $bench}' \
    > "$run/run.json"
  if [ -n "$panel" ]; then
    log "run $name: roborev $ref ($sha), panel $panel"
  else
    log "run $name: roborev $ref ($sha), agent $agent ${agent_provider:+provider $agent_provider }${model:+model $model }reasoning $reasoning"
  fi

  count=$(jq length "$list")
  if [ ${#prs[@]} -gt 0 ]; then indices="${prs[*]}"; else indices="$(seq 0 $((count - 1)))"; fi
  local keys=()
  for i in $indices; do
    if key="$(prepare_pr "$(jq -c ".[$i]" "$list")" "$run" "$prdir")"; then
      keys+=("$key")
    else
      failed=$((failed + 1))
    fi
  done

  local running=0
  for key in "${keys[@]}"; do
    review_pr "$run" "$prdir" "$key" &
    running=$((running + 1))
    if [ "$running" -ge "$jobs" ]; then
      wait -n || failed=$((failed + 1))
      running=$((running - 1))
    fi
  done
  while [ "$running" -gt 0 ]; do
    wait -n || failed=$((failed + 1))
    running=$((running - 1))
  done

  for key in "${keys[@]}"; do
    git -C "$cache/repos/$(jq -r '.nwo | sub("/"; "_")' "$prdir/$key/pr.json")" \
      worktree remove --force "$prdir/$key/repo" 2>/dev/null || true
  done
  log "run $name: $(find "$run/findings" -name '*.json' | wc -l) reviewed, $failed failed"
  # The judge scores only the pull requests that have findings, so a failed
  # review would silently drop out of the score instead of counting as a miss.
  [ "$failed" -eq 0 ] || die "run $name is incomplete; rerun the same command to retry the failed pull requests"
}

judge() {
  [ -n "$name" ] || usage
  setup
  local run="$results/runs/$name"
  [ -d "$run/findings" ] || die "no findings for run $name"
  mkdir -p "$run/scoring"
  (cd "$bench" && npm run --silent judge -- \
    --candidate "$run/findings" --provider "$provider" --model "$judge_model" \
    --output "$run/scoring/results.json" --repo-dir "$cache/judge-repos" \
    --concurrency "$concurrency")
}

compare() {
  [ $# -ge 1 ] || usage
  local n
  for n in "$@"; do
    [ -f "$results/runs/$n/scoring/results.json" ] || die "run $n has not been judged"
  done
  printf '%-28s %5s %8s %6s %6s %6s %6s %6s %8s\n' run PRs findings GP GR F1 AP AR "high GR"
  for n in "$@"; do
    jq -r --arg n "$n" '
      def pct: if . == null then "-" else (. * 1000 | round / 10 | tostring) end;
      .micro.overall as $m
      | (if ($m.grounded_precision // 0) + ($m.grounded_recall // 0) > 0
         then 2 * $m.grounded_precision * $m.grounded_recall / ($m.grounded_precision + $m.grounded_recall)
         else null end) as $f1
      | [$n, .pr_count, $m.candidate_count, ($m.grounded_precision | pct), ($m.grounded_recall | pct),
         ($f1 | pct), ($m.augmented_precision | pct), ($m.augmented_recall | pct),
         (.micro.by_severity.high.grounded_recall | pct)]
      | @tsv' "$results/runs/$n/scoring/results.json" |
      awk -F'\t' '{ printf "%-28s %5s %8s %6s %6s %6s %6s %6s %8s\n", $1, $2, $3, $4, $5, $6, $7, $8, $9 }'
  done
  echo
  echo "Micro averages over all findings. GP/GR: grounded precision and recall against the golden set."
  echo "AP/AR: augmented, also crediting unmatched findings the judge classified as valid."
  if [ $# -eq 2 ]; then
    echo
    echo "Golden findings covered per pull request ($1 -> $2):"
    jq -rn --slurpfile a "$results/runs/$1/scoring/results.json" --slurpfile b "$results/runs/$2/scoring/results.json" '
      ($a[0].per_pr | map({key: .pr_key, value: .metrics.overall.golden_covered_count}) | from_entries) as $x
      | $b[0].per_pr[]
      | select(($x[.pr_key] // 0) != .metrics.overall.golden_covered_count)
      | "  \(.pr_key): \($x[.pr_key] // 0) -> \(.metrics.overall.golden_covered_count) of \(.metrics.overall.golden_tp_count)"'
  fi
}

ab() {
  [ -n "$base_ref" ] && [ -n "$head_ref" ] || usage
  local stamp
  stamp="$(date +%Y%m%d-%H%M%S)"
  local base_name="${name:-ab-$stamp}-base" head_name="${name:-ab-$stamp}-head"
  ref="$base_ref" name="$base_name" run_eval
  ref="$head_ref" name="$head_name" run_eval
  name="$base_name" judge
  name="$head_name" judge
  compare "$base_name" "$head_name"
}

harness() {
  [ -n "$ref" ] && [ -n "$name" ] && [ ${#arms[@]} -ge 2 ] || usage
  local arm names=() arm_name
  for arm in "${arms[@]}"; do
    IFS=: read -r agent agent_provider model <<<"$arm"
    [ -n "$agent" ] || die "--arm needs an agent: $arm"
    arm_name="$name-$agent${agent_provider:+-$agent_provider}${model:+-$model}"
    names+=("$arm_name")
    name="$arm_name" run_eval
  done
  for arm_name in "${names[@]}"; do
    name="$arm_name" judge
  done
  compare "${names[@]}"
}

panel_eval() {
  [ -n "$ref" ] && [ -n "$name" ] && [ -n "$panel" ] || usage
  [ -f "$panel_config" ] || die "no panel config at $panel_config"
  run_eval
  # Each member's findings become a run of their own, so members are judged
  # exactly like the panel and can be compared with it.
  local run="$results/runs/$name" member names=("$name") f
  for f in "$run"/member-findings/*/*.json; do
    [ -e "$f" ] || continue
    member="$(basename "$f" .json)"
    mkdir -p "$results/runs/$name-$member/findings"
    cp "$f" "$results/runs/$name-$member/findings/$(basename "$(dirname "$f")").json"
  done
  for f in "$results/runs/$name"-*; do
    [ -d "$f/findings" ] && names+=("$(basename "$f")")
  done
  local n
  for n in "${names[@]}"; do
    name="$n" judge &
  done
  wait
  compare ${baseline:+"$baseline"} "${names[@]}"
}

[ $# -ge 1 ] || usage
command="$1"; shift
case "$command" in
  setup) setup ;;
  run) parse_options "$@"; run_eval ;;
  judge) parse_options "$@"; judge ;;
  compare) compare "$@" ;;
  ab) parse_options "$@"; ab ;;
  harness) parse_options "$@"; harness ;;
  panel) parse_options "$@"; panel_eval ;;
  *) usage ;;
esac
