#!/usr/bin/env bash
# Run prebuilt otelbench against explicitly owned, already-running local services.
# Raw output stays outside the frozen worktree; archive it after the campaign.
set -euo pipefail
root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
: "${BIN:?set BIN to the prebuilt otelbench executable}"
: "${FDB_CLUSTER_FILE:?set FDB_CLUSTER_FILE to an owned cluster file}"
: "${CH_URL:?set CH_URL to an owned ClickHouse HTTP endpoint}"
: "${OUT:?set OUT to a fresh output directory outside the worktree}"
: "${RUN_PREFIX:?set RUN_PREFIX to a fresh lowercase run namespace}"
[[ $RUN_PREFIX =~ ^[a-z0-9_]{8,30}$ ]] || { echo 'invalid RUN_PREFIX' >&2; exit 2; }
[[ -x $BIN && -f $FDB_CLUSTER_FILE ]] || { echo 'missing executable or cluster file' >&2; exit 2; }
BIN=$(realpath "$BIN")
FDB_CLUSTER_FILE=$(realpath "$FDB_CLUSTER_FILE")
[[ ! -e $OUT ]] || { echo 'OUT already exists; refusing to overwrite evidence' >&2; exit 2; }
OUT=$(realpath -m "$OUT")
case "$OUT/" in "$root/"*) echo 'OUT must be outside the frozen worktree' >&2; exit 2 ;; esac
mkdir -p "$OUT"
count=${COUNT:-100000}
attrs=${ATTRS:-8}
tenants=${TENANTS:-10}
iterations=${ITERATIONS:-1000}
trace_iterations=${TRACE_ITERATIONS:-300}
rounds=${ROUNDS:-3}
max_candidates=${MAX_CANDIDATES:-2000000}
max_bytes=${MAX_BYTES:-268435456}
for n in "$count" "$attrs" "$tenants" "$iterations" "$trace_iterations" "$rounds" "$max_candidates" "$max_bytes"; do
  [[ $n =~ ^[1-9][0-9]*$ ]] || { echo 'numeric settings must be positive integers' >&2; exit 2; }
done
exec 9>"${FDB_LANE_LOCK:-/tmp/fdb-heavy-lane.lock}"
flock 9
cd "$root"
[[ -z $(git status --porcelain=v1) ]] || { echo 'campaign requires a clean committed worktree' >&2; exit 3; }
revision=$(git rev-parse HEAD)
printf '%s\n' "$revision" > "$OUT/revision"
sha256sum "$BIN" > "$OUT/binary.sha256"
manifest() {
  git ls-files --cached --others --exclude-standard -z | LC_ALL=C sort -z | xargs -0 sha256sum
}
manifest > "$OUT/inputs.before"
printf 'count=%s attrs=%s tenants=%s iterations=%s trace_iterations=%s rounds=%s max_candidates=%s max_bytes=%s\n' \
  "$count" "$attrs" "$tenants" "$iterations" "$trace_iterations" "$rounds" "$max_candidates" "$max_bytes" > "$OUT/parameters"
printf '%s\n' 'single-node immutable corpus; warm-cache closed-loop; no HA, live-ingest, or SLO claim' >> "$OUT/parameters"
failures=0
finish() {
  local rc=$?
  manifest > "$OUT/inputs.after"
  if ! cmp -s "$OUT/inputs.before" "$OUT/inputs.after" || [[ $(git rev-parse HEAD) != "$revision" ]] || [[ -n $(git status --porcelain=v1) ]]; then
    echo 'INVALID: source inputs, revision, or worktree status changed' >&2
    rc=4
  fi
  printf 'exit=%s query_command_failures=%s end=%s\n' "$rc" "$failures" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$OUT/status"
  trap - EXIT
  exit "$rc"
}
trap finish EXIT
arms=(fdb_trace fdb_time ch_map ch_attrs_trace ch_attrs_time)
settings() {
  local arm=$1
  common=(--run "${RUN_PREFIX}_${arm}" --count "$count" --attrs "$attrs" --tenants "$tenants"
    --seed 283 --match-every 100 --k 100 --revision "$revision"
    --max-candidates "$max_candidates" --max-bytes "$max_bytes" --timeout 60m)
  case "$arm" in
    fdb_trace) common+=(--backend fdb --order trace --cluster-file "$FDB_CLUSTER_FILE") ;;
    fdb_time) common+=(--backend fdb --order time --cluster-file "$FDB_CLUSTER_FILE") ;;
    ch_map) common+=(--backend ch-map --order trace --ch-url "$CH_URL") ;;
    ch_attrs_trace) common+=(--backend ch-attrs --order trace --ch-url "$CH_URL") ;;
    ch_attrs_time) common+=(--backend ch-attrs --order time --ch-url "$CH_URL") ;;
  esac
}
for arm in "${arms[@]}"; do
  settings "$arm"
  printf '%s load %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$arm"
  "$BIN" "${common[@]}" --mode ingest --output "$OUT/${arm}.ingest.json"
  start=$(date +%s%N)
  if [[ $arm == ch_* ]]; then
    # Favor a mature static-corpus CH layout; disclose this maintenance cost.
    database="otel_${RUN_PREFIX}_${arm}"
    curl --fail --silent --show-error "$CH_URL" --data-binary "OPTIMIZE TABLE ${database}.spans FINAL" > "$OUT/${arm}.optimize.log"
    if [[ $arm != ch_map ]]; then
      curl --fail --silent --show-error "$CH_URL" --data-binary "OPTIMIZE TABLE ${database}.attr_lookup FINAL" >> "$OUT/${arm}.optimize.log"
    fi
  fi
  sleep 5
  printf '%s\n' "$(( $(date +%s%N) - start ))" > "$OUT/${arm}.stabilization_ns"
  "$BIN" "${common[@]}" --mode audit --output "$OUT/${arm}.audit.json"
done
for ((round=0; round<rounds; round++)); do
  for ((offset=0; offset<${#arms[@]}; offset++)); do
    arm=${arms[$(( (round + offset) % ${#arms[@]} ))]}
    settings "$arm"
    printf '%s query round=%s arm=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$round" "$arm"
    if ! "$BIN" "${common[@]}" --mode query --suite selective --iterations "$iterations" --output "$OUT/${arm}.r${round}.selective.json"; then
      failures=$((failures + 1))
    fi
    if ! "$BIN" "${common[@]}" --mode query --suite trace --iterations "$trace_iterations" --output "$OUT/${arm}.r${round}.trace.json"; then
      failures=$((failures + 1))
    fi
  done
done
(( failures == 0 ))
