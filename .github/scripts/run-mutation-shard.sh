#!/usr/bin/env bash
# Mutation Hunt の1 shardを実行し、測定manifest・実行結果・診断を分離して保存する。
set -uo pipefail

mkdir -p mutation-results mutation-artifacts
read -r -a packages <<<"$HUNT_PACKAGES"
matrix_shard_files=()
if [[ -n ${HUNT_SHARD_FILES:-} ]]; then read -r -a matrix_shard_files <<<"$HUNT_SHARD_FILES"; fi
matrix_exclude_files=()
if [[ -n ${HUNT_EXCLUDE_FILES:-} ]]; then read -r -a matrix_exclude_files <<<"$HUNT_EXCLUDE_FILES"; fi
measurement_timeout=$(((HUNT_JOB_TIMEOUT - 20) * 60))
# 変異で暴走したtest processがrunnerのメモリを使い切るとjobごと停止し、結果が残らない。
# Gremlins配下のprocess全体の合計に上限を掛け、runner agentとOS用の余裕を残す。
# 上限で停止した変異はテスト失敗として扱われる。
memory_total_bytes=$(($(awk '/^MemTotal:/ {print $2}' /proc/meminfo) * 1024))
mutation_memory_limit=$((memory_total_bytes - 3 * 1024 * 1024 * 1024))
failures=0

write_failure() {
  local output=$1 profile=$2 status=$3 stage=$4 detail=$5 exit_code=${6:-1}
  jq -n --arg run "$HUNT_RUN_ID" --arg attempt "$HUNT_RUN_ATTEMPT" --arg sha "$GITHUB_SHA" \
    --arg shard "$HUNT_ID" --arg profile "$profile" --arg status "$status" --arg stage "$stage" \
    --arg detail "$detail" --argjson exit_code "$exit_code" \
    '{schema_version:1,run_id:$run,run_attempt:$attempt,test_sha:$sha,shard:$shard,profile:$profile,stage:$stage,status:$status,exit_code:$exit_code,signal:null,duration_seconds:0,detail:$detail}' \
    >"$output"
}

for package in "${packages[@]}"; do
  profile=${package#./}
  safe_profile=${profile//\//-}
  result="mutation-results/$safe_profile.json"
  dry_result="mutation-results/$safe_profile-dry-run.json"
  manifest="mutation-artifacts/$safe_profile/manifest.json"
  execution="mutation-artifacts/$safe_profile/execution.json"
  diagnostics="mutation-artifacts/$safe_profile/diagnostics"
  mkdir -p "$diagnostics"
  shard_files=("${matrix_shard_files[@]}")
  exclude_files=("${matrix_exclude_files[@]}")

  if [[ $HUNT_SHARD_COUNT -gt 1 ]]; then
    dry_args=(unleash "$package" --dry-run -o "$dry_result" --workers "$HUNT_WORKERS" --timeout-coefficient "$HUNT_TIMEOUT_COEFFICIENT")
    if [[ $HUNT_MUTATORS == boundary ]]; then
      dry_args+=(--arithmetic-base=false --conditionals-boundary=true --conditionals-negation=false --increment-decrement=true --invert-negatives=false --invert-assignments=false --invert-bitwise=false --invert-bwassign=false --invert-logical=false --invert-loopctrl=false --remove-self-assignments=false)
    fi
    printf '%s\n' "${dry_args[@]}" >"$diagnostics/dry-run-command.txt"
    timeout --signal=TERM --kill-after=10s 10m .tools/bin/gremlins "${dry_args[@]}" >"$diagnostics/dry-run.stdout.log" 2>"$diagnostics/dry-run.stderr.log"
    dry_status=$?
    if [[ $dry_status -ne 0 || ! -s $dry_result ]]; then
      write_failure "$execution" "$profile" gremlins_failed setup "dry-run failed or produced no result" "$dry_status"
      echo "::error title=Gremlins dry-run failed::$package (exit $dry_status)"
      failures=$((failures + 1))
      continue
    fi
    shard_json=$(.tools/bin/mutationshard -input "$dry_result" -count "$HUNT_SHARD_COUNT" -index "$HUNT_SHARD_INDEX")
    if ! jq -e '(.files | type == "array" and length > 0) and (.exclude_files | type == "array")' >/dev/null <<<"$shard_json"; then
      write_failure "$execution" "$profile" result_invalid setup "dynamic shard plan is invalid"
      echo "::error title=Mutation shard planning failed::$package returned an invalid file set"
      failures=$((failures + 1))
      continue
    fi
    mapfile -t shard_file_names < <(jq -r '.files[]' <<<"$shard_json")
    shard_files=()
    for shard_file in "${shard_file_names[@]}"; do shard_files+=("$profile/$shard_file"); done
    mapfile -t exclude_files < <(jq -r '.exclude_files[]?' <<<"$shard_json")
  fi

  # 固定file shardもdry-runを残し、変異0件と結果欠落を区別する。
  if [[ $HUNT_SHARD_COUNT -eq 1 ]]; then
    dry_args=(unleash "$package" --dry-run -o "$dry_result" --workers "$HUNT_WORKERS" --timeout-coefficient "$HUNT_TIMEOUT_COEFFICIENT")
    for exclude_file in "${exclude_files[@]}"; do dry_args+=(--exclude-files "$exclude_file"); done
    if [[ $HUNT_MUTATORS == boundary ]]; then
      dry_args+=(--arithmetic-base=false --conditionals-boundary=true --conditionals-negation=false --increment-decrement=true --invert-negatives=false --invert-assignments=false --invert-bitwise=false --invert-bwassign=false --invert-logical=false --invert-loopctrl=false --remove-self-assignments=false)
    fi
    printf '%s\n' "${dry_args[@]}" >"$diagnostics/dry-run-command.txt"
    timeout --signal=TERM --kill-after=10s 10m .tools/bin/gremlins "${dry_args[@]}" >"$diagnostics/dry-run.stdout.log" 2>"$diagnostics/dry-run.stderr.log"
    dry_status=$?
    if [[ $dry_status -ne 0 ]]; then
      write_failure "$execution" "$profile" gremlins_failed setup "dry-run failed or produced no result" "$dry_status"
      echo "::error title=Gremlins dry-run failed::$package (exit $dry_status)"
      failures=$((failures + 1))
      continue
    fi
    if [[ ! -s $dry_result ]]; then printf '{"files":[]}\n' >"$dry_result"; fi
  fi
  cp "$dry_result" "$diagnostics/dry-run.json"
  runnable=$(jq '[.files[]?.mutations[]? | select(.status == "RUNNABLE")] | length' "$dry_result")
  allow_missing=false
  if [[ $runnable -eq 0 ]]; then allow_missing=true; fi

  if ! go clean -testcache; then
    write_failure "$execution" "$profile" gremlins_failed setup "Go test cache reset failed"
    echo "::error title=Mutation test cache reset failed::$package"
    failures=$((failures + 1))
    continue
  fi
  args=(unleash "$package" -o "$result" --workers "$HUNT_WORKERS" --timeout-coefficient "$HUNT_TIMEOUT_COEFFICIENT")
  for exclude_file in "${exclude_files[@]}"; do args+=(--exclude-files "$exclude_file"); done
  if [[ $HUNT_MUTATORS == boundary ]]; then
    args+=(--arithmetic-base=false --conditionals-boundary=true --conditionals-negation=false --increment-decrement=true --invert-negatives=false --invert-assignments=false --invert-bitwise=false --invert-bwassign=false --invert-logical=false --invert-loopctrl=false --remove-self-assignments=false)
  fi
  jq -n --arg sha "$GITHUB_SHA" --arg shard "$HUNT_ID" --arg profile "$profile" \
    --arg files "${shard_files[*]}" --arg mutators "$HUNT_MUTATORS" --arg workers "$HUNT_WORKERS" \
    --arg coefficient "$HUNT_TIMEOUT_COEFFICIENT" \
    '{schema_version:1,test_sha:$sha,shard:$shard,profile:$profile,assigned_files:($files|split(" ")|map(select(length>0))),mutators:$mutators,workers:($workers|tonumber),timeout_coefficient:($coefficient|tonumber)}' \
    >"$diagnostics/preflight.json"

  node .github/scripts/mutation-runner.cjs \
    --timeout-seconds "$measurement_timeout" --grace-seconds 10 \
    --allow-missing-result "$allow_missing" \
    --output "$result" --execution-result "$execution" --diagnostics-dir "$diagnostics" \
    --shard "$HUNT_ID" --profile "$profile" --run-id "$HUNT_RUN_ID" \
    --attempt "$HUNT_RUN_ATTEMPT" --sha "$GITHUB_SHA" -- \
    bash .github/scripts/with-memory-cgroup.sh "mutation-hunt-$safe_profile" "$mutation_memory_limit" -- \
    .tools/bin/gremlins "${args[@]}"
  runner_status=$?
  cp "/sys/fs/cgroup/mutation-hunt-$safe_profile/memory.events" "$diagnostics/memory-events.txt" 2>/dev/null || true
  cp "/sys/fs/cgroup/mutation-hunt-$safe_profile/memory.peak" "$diagnostics/memory-peak.txt" 2>/dev/null || true
  if [[ $runner_status -ne 0 ]]; then
    echo "::error title=Mutation measurement failed::$package ($(jq -r '.status' "$execution" 2>/dev/null || echo unknown))"
    failures=$((failures + 1))
    continue
  fi

  duration=$(jq -r '.duration_seconds' "$execution")
  command_string="gremlins ${args[*]}"
  report_args=(-root "$GITHUB_WORKSPACE" -profile "$profile" -mutators "$HUNT_MUTATORS" -output "$manifest" -run-id "$HUNT_RUN_ID" -run-attempt "$HUNT_RUN_ATTEMPT" -duration-seconds "$duration" -command "$command_string")
  if [[ -s $result ]]; then report_args+=(-input "$result"); else report_args+=(-empty-result); fi
  if [[ ${#shard_files[@]} -gt 0 ]]; then report_args+=(-shard-files "${shard_files[*]}"); fi
  .tools/bin/mutationreport "${report_args[@]}" 2>"$diagnostics/mutationreport.stderr.log"
  report_status=$?
  if [[ $report_status -ne 0 ]]; then
    status=result_invalid
    if grep -q 'exclusion .* resolved to mutation .* absent' "$diagnostics/mutationreport.stderr.log"; then status=exclusion_mismatch; fi
    jq --arg status "$status" --arg detail "mutationreport exit $report_status" '.stage="report" | .status=$status | .detail=$detail' "$execution" >"$execution.tmp"
    mv "$execution.tmp" "$execution"
    echo "::error title=Mutation report failed::$package (exit $report_status)"
    failures=$((failures + 1))
  fi
done

{
  echo "## Mutation hunt: \`$HUNT_ID\`"
  echo
  echo "- Packages: \`$HUNT_PACKAGES\`"
  echo "- Measurement failures: $failures"
  jq -rs '[.[].survivors[]] | unique_by(.id) | "- Unexcluded survivors: " + (length|tostring)' mutation-artifacts/*/manifest.json 2>/dev/null || true
} >>"$GITHUB_STEP_SUMMARY"

test "$failures" -eq 0
