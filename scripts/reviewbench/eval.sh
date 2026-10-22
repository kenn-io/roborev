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
#   scripts/reviewbench/eval.sh coverage BASE NAME...
#   scripts/reviewbench/eval.sh retention SOURCE NAME...
#   scripts/reviewbench/eval.sh ab --base REF --head REF [run and judge options]
#   scripts/reviewbench/eval.sh harness --ref REF --name NAME --arm A --arm B... [options]
#   scripts/reviewbench/eval.sh panel --ref REF --name NAME --panel PANEL [--baseline RUN] [options]
#   scripts/reviewbench/eval.sh panel ... --members-from RUN [--members-from RUN...]
#
# ab compares two refs with one agent. harness compares agents on one ref:
# each --arm is AGENT:PROVIDER:MODEL, for example codex::gpt-6-sol or
# pi:cpa:gpt-6-sol; PROVIDER only applies to pi and may be empty. panel reviews
# with a roborev panel from --panel-config (default ~/.roborev/config.toml),
# then judges the panel and each member on its own; --baseline names an
# earlier run, such as a stock reviewer, to print beside them. With
# --members-from, the panel does not run its members: it synthesizes the
# reviews they saved in those earlier panel runs, so synthesis with and without
# a member can be compared on the same member reviews. A focused reviewer is a
# one-member panel, which reports its findings without synthesis.
# --synthesis-instructions FILE replaces the synthesis prompt's instructions
# with the file's text, and --synthesis concat skips synthesis and keeps every
# member finding unmerged, as a reference for how much synthesis could keep.
#
# Run options:
#   --agent NAME        roborev agent (default claude-code)
#   --model MODEL       agent model (default: the agent's own default)
#   --agent-provider P  pi provider for the model
#   --reasoning LEVEL   fast, standard, or thorough (default thorough)
#   --jobs N            reviews run at once (default 4)
#   --set SET           test: the 25-PR test set (default); full: all 219 PRs;
#                       or NAME, the PR list in scripts/reviewbench/sets/NAME.txt
#   --pr INDEX          only this manifest index; repeat for several
#   --fresh             run every review, even ones the review store already has
# Judge options:
#   --provider NAME     pi provider for the judge (default anthropic)
#   --judge-model ID    judge model (default claude-sonnet-5, the leaderboard's)
#   --concurrency N     judge calls at once (default 4)
#
# Every review is kept in the review store, $ROBOREV_EVAL_DIR/reviews, at
# HARNESS/MODEL/REASONING/PROMPT_VERSION/REVIEWER/PR/N. A run reuses a stored
# review when the complete prompt it would send has the same hash, and runs
# only the reviews the store lacks; --fresh adds new samples instead. A run's
# samples.tsv lists the stored review each pull request used.
#
# The judge reaches models through your pi login, settings, and extensions in
# ~/.pi/agent, the same way the pi command line does.
# Results (findings, scores, logs, reports) live in $ROBOREV_EVAL_DIR, by
# default ~/.local/share/roborev-reviewbench. Benchmark repositories and
# checkouts are disposable and live in $ROBOREV_EVAL_CACHE, by default
# /var/tmp/roborev-reviewbench.
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
# /var/tmp, not $TMPDIR: /tmp is often RAM-backed, and the cache holds about
# a hundred repository clones plus a checkout of every pull request per run.
# /var/tmp is a temporary directory on disk.
cache="${ROBOREV_EVAL_CACHE:-/var/tmp/roborev-reviewbench}"
bench="$cache/ReviewBench"

usage() { sed -n '8,41p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
die() { echo "eval: $*" >&2; exit 1; }
log() { echo "eval: $*" >&2; }

agent=claude-code model="" agent_provider="" reasoning=thorough jobs=4 set_name=test prs=() arms=()
panel="" panel_config="$HOME/.roborev/config.toml" baseline="" members_from=()
synthesis="" synthesis_instructions="" fresh=""
provider=anthropic judge_model=claude-sonnet-5 concurrency=4
ref="" name="" base_ref="" head_ref=""

parse_options() {
  while [ $# -gt 0 ]; do
    if [ "$1" = --fresh ]; then fresh=1; shift; continue; fi
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
      --members-from) members_from+=("$2") ;;
      --synthesis) synthesis="$2" ;;
      --synthesis-instructions) synthesis_instructions="$(realpath "$2")" ;;
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
    *)
      # A named set lists PR URLs, one per line; '#' lines are comments.
      local file="$root/scripts/reviewbench/sets/$set_name.txt" out="$cache/sets/$set_name.json"
      [ -f "$file" ] || die "--set must be test, full, or a list in scripts/reviewbench/sets/"
      mkdir -p "$cache/sets"
      grep -v '^#' "$file" | grep . | jq -R . | jq -s --slurpfile m "$bench/corpus/manifest.json" \
        'map(. as $u | $m[0][] | select(.pr_url == $u))' > "$out"
      [ "$(jq length "$out")" -eq "$(grep -v '^#' "$file" | grep -c .)" ] ||
        die "set $set_name lists PRs that are not in ReviewBench's manifest"
      echo "$out"
      ;;
  esac
}

# prompt_version SHA names the review prompt at roborev commit SHA by the last
# commit that changed what a reviewer is sent or how its findings are kept.
# It groups the review store; the prompt hash decides reuse.
prompt_version() {
  echo "p-$(git -C "$root" log -1 --format=%H "$1" -- internal/prompt internal/review internal/agent \
    pkg/structuredreview ':(exclude)internal/review/synthes*' ':(exclude)*_test.go' | cut -c1-8)"
}

# build_probe REF OUT builds the probe against REF's own review code. The
# probe source comes from this checkout, so refs that predate it work too.
build_probe() {
  local sha tree
  sha="$(git -C "$root" rev-parse --verify "$1^{commit}")" || die "unknown ref $1"
  mkdir -p "$cache"
  tree="$(mktemp -d -p "$cache" build.XXXXXX)"
  git -C "$root" worktree add -q --detach "$tree/src" "$sha"
  mkdir -p "$tree/src/scripts/reviewbench/probe"
  find "$probe_src" -maxdepth 1 -name '*.go' ! -name '*_test.go' -exec cp {} "$tree/src/scripts/reviewbench/probe/" \;
  # Refs from before pull request context cannot show it to the reviewer.
  local tags=()
  grep -q 'func (b \*Builder) WithPullRequest' "$tree/src/internal/prompt/prompt.go" || tags=(-tags nopullrequest)
  if ! (cd "$tree/src" && go build "${tags[@]}" -o "$2" ./scripts/reviewbench/probe); then
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
    GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" fetch -q --filter=tree:0 origin "$base" "$head" 2>/dev/null || true
  fi
  if ! has_commits "$repo" "$base" "$head"; then
    GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" fetch -q --filter=tree:0 upstream "$base" "pull/$pr/head" 2>/dev/null \
      || GIT_LFS_SKIP_SMUDGE=1 git -C "$repo" fetch -q --filter=tree:0 upstream "$base" "$head" 2>/dev/null || true
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
  local store=(-store "$results/reviews" -prompt-version "$prompt_ver" -roborev-sha "$sha"
    -pr-key "$key" -run-name "$name" ${fresh:+-fresh})
  if [ -n "$panel" ]; then
    mode=(-panel "$panel" -member-out "$run/member-findings/$key")
    if [ ${#members_from[@]} -gt 0 ]; then
      local dirs=() from
      for from in "${members_from[@]}"; do dirs+=("$results/runs/$from/artifacts/$key/members"); done
      mode=(-panel "$panel" -members-from "$(IFS=,; echo "${dirs[*]}")")
      store=()
    fi
    [ -z "$synthesis" ] || mode+=(-synthesis "$synthesis")
    [ -z "$synthesis_instructions" ] || mode+=(-synthesis-instructions "$synthesis_instructions")
  fi
  for attempt in 1 2; do
    if ROBOREV_DATA_DIR="$run/roborev-data" timeout "${PR_TIMEOUT:-1800}" \
      "$run/bin/probe" -repo "$prdir/$key/repo" -pr-json "$prdir/$key/pr.json" \
      "${mode[@]}" "${store[@]}" -producer "roborev" \
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
  if [ -f "$run/run.json" ]; then
    # A resumed run must keep reviewing with the code it started with, or
    # its pull requests would mix reviews from two versions.
    local started_sha
    started_sha="$(jq -r .sha "$run/run.json")"
    [ "$(git -C "$root" rev-parse --verify "$ref^{commit}")" = "$started_sha" ] ||
      die "run $name started at $started_sha; resume it with --ref $started_sha"
  fi
  sha="$(build_probe "$ref" "$run/bin/probe")"
  local prompt_ver
  prompt_ver="$(prompt_version "$sha")"
  if [ -n "$panel" ]; then
    # Only the review sections: panels, subagents, and review types. Nothing
    # else from the user's roborev config reaches the eval.
    awk '/^\[/{keep = ($0 ~ /^\[review[].]/)} keep' "$panel_config" > "$run/roborev-data/config.toml"
  fi
  list="$(manifest)"
  jq -n --arg ref "$ref" --arg sha "$sha" --arg agent "$agent" --arg model "$model" \
    --arg provider "$agent_provider" --arg reasoning "$reasoning" --arg set "$set_name" \
    --arg bench "$REVIEWBENCH_SHA" \
    --arg prompt_version "$prompt_ver" --arg fresh "$fresh" \
    --arg panel "$panel" --arg synthesis "$synthesis" --arg synthesis_instructions "$synthesis_instructions" --args \
    '{ref: $ref, sha: $sha, prompt_version: $prompt_version, fresh: ($fresh != ""), agent: $agent, provider: $provider, model: $model, reasoning: $reasoning, panel: $panel, members_from: $ARGS.positional, synthesis: $synthesis, synthesis_instructions: $synthesis_instructions, set: $set, reviewbench: $bench}' \
    "${members_from[@]}" \
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
  # The run's recipe: the stored review each pull request, or member, used.
  if [ -d "$run/artifacts" ]; then
    (cd "$run/artifacts" && find . -name sample -printf '%h\t' -exec cat {} \;) |
      sed 's#^\./##' | sort > "$run/samples.tsv"
  fi
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

# golden_tp_map writes, for every golden file, the indices of its true-positive
# findings. Judge results index the full golden list, which also holds the
# benchmark's known false positives; a candidate can cover one of those.
golden_tp_map() {
  jq -n '[inputs | {(.pr_key): [.findings | to_entries[] | select(.value.tp_fp == "tp") | .key]}] | add' "$bench"/golden/*.json
}

# coverage BASE NAME... counts, for each judged run, the golden findings it
# covers that BASE misses and the ones BASE covers that it misses. Use it to
# see what a focused reviewer could add to a panel.
coverage() {
  [ $# -ge 2 ] || usage
  local base="$1" n
  shift
  for n in "$base" "$@"; do
    [ -f "$results/runs/$n/scoring/results.details.json" ] || die "run $n has not been judged"
  done
  local tp
  tp="$(mktemp)"
  golden_tp_map > "$tp"
  printf '%-28s %8s %6s %6s %8s\n' run covered adds loses together
  for n in "$@"; do
    jq -rn --arg n "$n" --slurpfile tp "$tp" \
      --slurpfile a "$results/runs/$base/scoring/results.details.json" \
      --slurpfile b "$results/runs/$n/scoring/results.details.json" \
      --slurpfile s "$results/runs/$base/scoring/results.json" '
      def cov(d): d | map(.pr_key as $k | {key: $k, value: ([.candidate_findings[] | select(.status == "matched_tp") | .covered_golden_indices[]?] | unique | map(select(. as $i | $tp[0][$k] // [] | index($i))))}) | from_entries;
      cov($a[0]) as $x | cov($b[0]) as $y
      | ([$s[0].per_pr[].metrics.overall.golden_tp_count] | add) as $total
      | ($x + $y | keys) as $keys
      | [$n,
         ([$y[]] | map(length) | add // 0),
         ([$keys[] as $k | (($y[$k] // []) - ($x[$k] // [])) | length] | add),
         ([$keys[] as $k | (($x[$k] // []) - ($y[$k] // [])) | length] | add),
         "\([$keys[] as $k | (($x[$k] // []) + ($y[$k] // []) | unique | length)] | add)/\($total)"]
      | @tsv' | awk -F'\t' '{ printf "%-28s %8s %6s %6s %8s\n", $1, $2, $3, $4, $5 }'
  done
  echo
  echo "covered: golden findings the run matched. adds: matched by the run but not by $base."
  echo "loses: matched by $base but not by the run. together: matched by either."
  echo "Only true-positive golden findings count. Totals can differ by one or two from the judge's golden_covered_count."
  rm -f "$tp"
}

# retention SOURCE NAME... measures synthesis on member reviews saved by the
# panel run SOURCE: how many of the golden findings its members matched each
# run keeps. The judge credits a golden finding once and counts a duplicate
# as a novel finding, so duplicates do not lower any score; count them from the
# findings themselves.
retention() {
  [ $# -ge 2 ] || usage
  local source="$1" member members=() n
  shift
  for member in "$results/runs/$source"/member-findings/*/*.json; do
    [ -e "$member" ] || continue
    member="$(basename "$member" .json)"
    [[ " ${members[*]} " == *" $member "* ]] || members+=("$member")
  done
  [ ${#members[@]} -gt 0 ] || die "run $source saved no member findings"
  local files=()
  for member in "${members[@]}"; do files+=("$results/runs/$source-$member/scoring/results.details.json"); done
  local tp
  tp="$(mktemp)"
  golden_tp_map > "$tp"
  printf '%-28s %8s %8s\n' run findings kept
  for n in "$@"; do
    jq -rn --arg n "$n" --slurpfile tp "$tp" --slurpfile run "$results/runs/$n/scoring/results.details.json" '
      def covered($k; c): [c[] | select(.status == "matched_tp") | .covered_golden_indices[]?] | unique | map(select(. as $i | $tp[0][$k] // [] | index($i)));
      ([inputs[] | .pr_key as $k | {($k): covered($k; .candidate_findings)}]
        | reduce .[] as $m ({}; reduce ($m | to_entries[]) as $e (.; .[$e.key] = ((.[$e.key] // []) + $e.value | unique)))) as $union
      | ($run[0] | map(.pr_key as $k | {key: $k, value: covered($k; .candidate_findings)}) | from_entries) as $mine
      | ([$union | to_entries[] | .value | length] | add) as $total
      | ([$union | to_entries[] | (.value - (.value - ($mine[.key] // []))) | length] | add) as $kept
      | [$n, ([$run[0][].candidate_findings | length] | add), "\($kept)/\($total)"] | @tsv' "${files[@]}" |
      awk -F'\t' '{ printf "%-28s %8s %8s\n", $1, $2, $3 }'
  done
  echo
  echo "kept: golden findings the members matched that the run also matched."
  rm -f "$tp"
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
  # exactly like the panel and can be compared with it. Reused member reviews
  # were already judged in the runs they came from.
  local run="$results/runs/$name" member names=("$name") f
  local members=()
  for f in "$run"/member-findings/*/*.json; do
    [ -e "$f" ] || continue
    member="$(basename "$f" .json)"
    [[ " ${members[*]} " == *" $member "* ]] || members+=("$member")
  done
  # A one-member panel's findings are that member's, so they are judged once.
  if [ ${#members[@]} -gt 1 ]; then
    for f in "$run"/member-findings/*/*.json; do
      member="$(basename "$f" .json)"
      mkdir -p "$results/runs/$name-$member/findings"
      cp "$f" "$results/runs/$name-$member/findings/$(basename "$(dirname "$f")").json"
    done
    for member in "${members[@]}"; do names+=("$name-$member"); done
  fi
  local n
  for n in "${names[@]}"; do
    name="$n" judge &
  done
  wait
  if [ -n "$baseline" ] && [ ! -f "$results/runs/$baseline/scoring/results.json" ]; then
    log "baseline $baseline has not been judged yet; comparing without it"
    baseline=""
  fi
  compare ${baseline:+"$baseline"} "${names[@]}"
}

[ $# -ge 1 ] || usage
command="$1"; shift
case "$command" in
  setup) setup ;;
  run) parse_options "$@"; run_eval ;;
  judge) parse_options "$@"; judge ;;
  compare) compare "$@" ;;
  coverage) coverage "$@" ;;
  retention) retention "$@" ;;
  ab) parse_options "$@"; ab ;;
  harness) parse_options "$@"; harness ;;
  panel) parse_options "$@"; panel_eval ;;
  *) usage ;;
esac
