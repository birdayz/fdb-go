#!/usr/bin/env bash
set -euo pipefail
source_script=${TEST_SRCDIR:?}/${TEST_WORKSPACE:?}/layers/otel/bench/local_campaign.sh
scratch=$(mktemp -d "${TEST_TMPDIR:?}/otel-campaign.XXXXXX")
campaign_pid=
cleanup() {
  if [[ -n $campaign_pid ]]; then
    kill "$campaign_pid" 2>/dev/null || true
    wait "$campaign_pid" 2>/dev/null || true
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
fixture=$scratch/repo
mkdir -p "$fixture/layers/otel/bench" "$scratch/bin" "$scratch/argv"
cp "$source_script" "$fixture/layers/otel/bench/local_campaign.sh"
script=$fixture/layers/otel/bench/local_campaign.sh
cat > "$fixture/fake-otelbench" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n ${LOCK_EVENT_PIPE:-} ]]; then printf 'executable\n' > "$LOCK_EVENT_PIPE"; fi
mode= output= backend= order= run=
invocation=$(wc -l < "${CALL_LOG:?}")
argv=("$@")
for ((i=0; i<${#argv[@]}; i+=2)); do
  printf '%s\t%s\n' "${argv[i]}" "${argv[i+1]}"
done | LC_ALL=C sort > "${ARG_DIR:?}/$invocation"
while (($#)); do
  case "$1" in
    --mode) mode=$2 ;;
    --output) output=$2 ;;
    --backend) backend=$2 ;;
    --order) order=$2 ;;
    --run) run=$2 ;;
  esac
  shift 2
done
printf '%s %s %s %s\n' "$mode" "$backend" "$order" "$run" >> "${CALL_LOG:?}"
printf '{"mode":"%s"}\n' "$mode" > "$output"
if [[ $mode == query && -n ${MUTATE_INPUT:-} ]]; then printf 'changed\n' > "$MUTATE_INPUT"; fi
if [[ $mode == query && -n ${MUTATE_HEAD:-} && ! -f $MUTATE_HEAD ]]; then
  git -C "${FIXTURE:?}" -c user.name=fixture -c user.email=fixture@example.invalid commit --allow-empty -qm changed-head
  touch "$MUTATE_HEAD"
fi
if [[ $mode == query && ${FAIL_QUERY:-0} == 1 ]]; then exit 9; fi
FAKE
cat > "$scratch/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >> "${CURL_LOG:?}"
printf '\n' >> "$CURL_LOG"
FAKE
cat > "$scratch/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
chmod +x "$fixture/fake-otelbench" "$scratch/bin/curl" "$scratch/bin/sleep"
git -C "$fixture" -c init.defaultBranch=fixture init -q
git -C "$fixture" add .
git -C "$fixture" -c user.name=fixture -c user.email=fixture@example.invalid commit -qm fixture
printf 'owned:owned@127.0.0.1:4500\n' > "$scratch/fdb.cluster"
export PATH="$scratch/bin:$PATH"
export BIN="$fixture/fake-otelbench" FDB_CLUSTER_FILE="$scratch/fdb.cluster" CH_URL=http://127.0.0.1:1
export RUN_PREFIX=otel_test COUNT=20 ATTRS=4 TENANTS=2 ITERATIONS=5 TRACE_ITERATIONS=4 ROUNDS=2
export MAX_CANDIDATES=1234 MAX_BYTES=567890
export FDB_LANE_LOCK="$scratch/lane.lock" FIXTURE="$fixture"
export CALL_LOG="$scratch/calls" CURL_LOG="$scratch/curl-calls" ARG_DIR="$scratch/argv"
: > "$CALL_LOG"
: > "$CURL_LOG"
case_number=0
expect_exit() {
  local expected=$1
  shift
  case_number=$((case_number + 1))
  local rc=0
  "$@" > "$scratch/case-${case_number}.log" 2>&1 || rc=$?
  if [[ $rc != "$expected" ]]; then
    cat "$scratch/case-${case_number}.log" >&2
    printf 'case %s: expected exit %s, got %s\n' "$case_number" "$expected" "$rc" >&2
    exit 1
  fi
}
# Admission failures cannot invoke the supplied executable or database endpoint.
expect_exit 1 env -u BIN OUT="$scratch/missing-bin" bash "$script"
expect_exit 2 env RUN_PREFIX='bad;namespace' OUT="$scratch/bad-prefix" bash "$script"
expect_exit 2 env BIN="$scratch/no-executable" OUT="$scratch/bad-bin" bash "$script"
expect_exit 2 env FDB_CLUSTER_FILE="$scratch/no-cluster" OUT="$scratch/bad-cluster" bash "$script"
expect_exit 2 env COUNT=0 OUT="$scratch/bad-number" bash "$script"
expect_exit 2 env OUT="$fixture/inside" bash "$script"
[[ ! -e $fixture/inside ]]
mkdir "$scratch/existing"
printf 'keep\n' > "$scratch/existing/sentinel"
expect_exit 2 env OUT="$scratch/existing" bash "$script"
[[ $(cat "$scratch/existing/sentinel") == keep ]]
touch "$fixture/dirty"
expect_exit 3 env OUT="$scratch/dirty" bash "$script"
rm "$fixture/dirty"
[[ ! -s $CALL_LOG && ! -s $CURL_LOG ]]
# All five layouts and both query modes execute in rotated order, retaining receipts.
expect_exit 0 env OUT="$scratch/success" bash "$script"
[[ $(grep -c '^ingest ' "$CALL_LOG") == 5 ]]
[[ $(grep -c '^audit ' "$CALL_LOG") == 5 ]]
[[ $(grep -c '^query ' "$CALL_LOG") == 20 ]]
[[ $(wc -l < "$CURL_LOG") == 5 ]]
[[ $(find "$scratch/success" -name '*.json' | wc -l) == 30 ]]
grep -q '^exit=0 query_command_failures=0 ' "$scratch/success/status"
cmp "$scratch/success/inputs.before" "$scratch/success/inputs.after"
# Independently pin every forwarded setting, suite, endpoint, output and rotation.
revision=$(git -C "$fixture" rev-parse HEAD)
assert_invocation() {
  local index=$1 arm=$2 mode=$3 suite=${4:-} iterations=${5:-}
  local expected=(--run "otel_test_${arm}" --count 20 --attrs 4 --tenants 2
    --seed 283 --match-every 100 --k 100 --revision "$revision"
    --max-candidates 1234 --max-bytes 567890 --timeout 60m)
  case "$arm" in
    fdb_trace) expected+=(--backend fdb --order trace --cluster-file "$scratch/fdb.cluster") ;;
    fdb_time) expected+=(--backend fdb --order time --cluster-file "$scratch/fdb.cluster") ;;
    ch_map) expected+=(--backend ch-map --order trace --ch-url http://127.0.0.1:1) ;;
    ch_attrs_trace) expected+=(--backend ch-attrs --order trace --ch-url http://127.0.0.1:1) ;;
    ch_attrs_time) expected+=(--backend ch-attrs --order time --ch-url http://127.0.0.1:1) ;;
    *) echo 'invalid expected arm' >&2; exit 1 ;;
  esac
  expected+=(--mode "$mode")
  if [[ $mode == query ]]; then
    expected+=(--suite "$suite" --iterations "$iterations" --output "$scratch/success/${arm}.r${round}.${suite}.json")
  else
    expected+=(--output "$scratch/success/${arm}.${mode}.json")
  fi
  for ((i=0; i<${#expected[@]}; i+=2)); do
    printf '%s\t%s\n' "${expected[i]}" "${expected[i+1]}"
  done | LC_ALL=C sort > "$scratch/expected-args"
  diff -u "$scratch/expected-args" "$ARG_DIR/$index"
}
arms=(fdb_trace fdb_time ch_map ch_attrs_trace ch_attrs_time)
index=0
for arm in "${arms[@]}"; do
  assert_invocation "$index" "$arm" ingest
  index=$((index + 1))
  assert_invocation "$index" "$arm" audit
  index=$((index + 1))
done
for ((round=0; round<2; round++)); do
  for ((offset=0; offset<5; offset++)); do
    arm=${arms[$(( (round + offset) % 5 ))]}
    assert_invocation "$index" "$arm" query selective 5
    index=$((index + 1))
    assert_invocation "$index" "$arm" query trace 4
    index=$((index + 1))
  done
done
[[ $index == 30 ]]
: > "$scratch/expected-curl"
for target in ch_map.spans ch_attrs_trace.spans ch_attrs_trace.attr_lookup ch_attrs_time.spans ch_attrs_time.attr_lookup; do
  printf '%q ' --fail --silent --show-error http://127.0.0.1:1 --data-binary "OPTIMIZE TABLE otel_otel_test_${target} FINAL" >> "$scratch/expected-curl"
  printf '\n' >> "$scratch/expected-curl"
done
diff -u "$scratch/expected-curl" "$CURL_LOG"
# An independent holder excludes a live contender until release, not just on paper.
case_number=$((case_number + 1))
export REAL_FLOCK
REAL_FLOCK=$(command -v flock)
cat > "$scratch/bin/flock" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n ${LOCK_EVENT_PIPE:-} ]]; then printf 'lock-attempt\n' > "$LOCK_EVENT_PIPE"; fi
exec "${REAL_FLOCK:?}" "$@"
FAKE
chmod +x "$scratch/bin/flock"
mkfifo "$scratch/lock-events"
exec 7<>"$scratch/lock-events"
exec 8>"$FDB_LANE_LOCK"
"$REAL_FLOCK" 8
mkdir "$scratch/lock-argv"
: > "$scratch/lock-calls"
: > "$scratch/lock-curl-calls"
timeout --kill-after=2s 20s env OUT="$scratch/contended" LOCK_EVENT_PIPE="$scratch/lock-events" \
  CALL_LOG="$scratch/lock-calls" CURL_LOG="$scratch/lock-curl-calls" ARG_DIR="$scratch/lock-argv" \
  bash "$script" 7>&- 8>&- > "$scratch/case-${case_number}.log" 2>&1 &
campaign_pid=$!
# The real flock is about to run; missing the lock call cannot pass by doing no work.
read -r -t 5 event <&7
[[ $event == lock-attempt ]]
kill -0 "$campaign_pid"
if read -r -t 0.5 event <&7; then
  printf 'campaign reached %s while the independent holder owned the lock\n' "$event" >&2
  exit 1
fi
[[ ! -s $scratch/lock-calls && ! -s $scratch/lock-curl-calls ]]
"$REAL_FLOCK" -u 8
exec 8>&-
read -r -t 5 event <&7
[[ $event == executable ]]
rc=0
wait "$campaign_pid" || rc=$?
campaign_pid=
if [[ $rc != 0 ]]; then
  cat "$scratch/case-${case_number}.log" >&2
  printf 'contending campaign failed after lock release: %s\n' "$rc" >&2
  exit 1
fi
exec 7>&-
[[ $(wc -l < "$scratch/lock-calls") == 30 ]]
[[ $(wc -l < "$scratch/lock-curl-calls") == 5 ]]
grep -q '^exit=0 query_command_failures=0 ' "$scratch/contended/status"
"$REAL_FLOCK" -n "$FDB_LANE_LOCK" true
# Failures remain raw and do not abort later arms.
expect_exit 1 env FAIL_QUERY=1 OUT="$scratch/query-failures" bash "$script"
grep -q '^exit=1 query_command_failures=20 ' "$scratch/query-failures/status"
[[ $(find "$scratch/query-failures" -name '*.json' | wc -l) == 30 ]]
# Both file mutations and HEAD-only mutations invalidate otherwise successful receipts.
expect_exit 4 env MUTATE_INPUT="$fixture/mutated" OUT="$scratch/input-change" bash "$script"
grep -q '^exit=4 ' "$scratch/input-change/status"
rm "$fixture/mutated"
expect_exit 4 env MUTATE_HEAD="$scratch/head-mutated" OUT="$scratch/head-change" bash "$script"
grep -q '^exit=4 ' "$scratch/head-change/status"
[[ -z $(git -C "$fixture" status --porcelain=v1) ]]
printf 'local campaign guard tests: %s cases passed\n' "$case_number"
