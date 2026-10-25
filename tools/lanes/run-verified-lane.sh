#!/usr/bin/env bash
# Run one validation lane on a clean worktree and prove its inputs did not change.
# Usage: run-verified-lane.sh <worktree> fast|race|full [extra just args...]
# FDB_LANE_LOCK serializes heavy lanes on a shared machine; evidence goes to a
# fresh directory under ${TMPDIR:-/tmp}, printed first. The lane's Bazel server is
# shut down afterwards; the exit status is the lane's.
set -euo pipefail
worktree=$1
lane=$2
shift 2
case "$lane" in
  fast) recipe=test ;;
  race) recipe=test-race ;;
  full) recipe=test-full ;;
  *) printf 'Unknown lane: %s\n' "$lane" >&2; exit 2 ;;
esac
cd "$worktree"
shutdown=(bazelisk shutdown)
if [[ $lane = race ]]; then
  shutdown=(bazelisk "--output_base=$(just --evaluate race_base)" shutdown)
fi
lock=${FDB_LANE_LOCK:-${TMPDIR:-/tmp}/fdb-heavy-lane.lock}
out=$(mktemp -d "${TMPDIR:-/tmp}/fdb-${lane}-evidence.XXXXXX")
printf '%s\n' "$out"
git rev-parse HEAD > "$out/head.before"
git status --porcelain=v1 > "$out/status.before"
if test -s "$out/status.before"; then
  cat "$out/status.before" >&2
  printf 'Refusing final lane on a dirty worktree\n' >&2
  exit 3
fi
manifest() {
  git ls-files --cached --others --exclude-standard -z |
    LC_ALL=C sort -z | xargs -0 sha256sum
}
manifest > "$out/inputs.before"
start=$(date +%s)
printf 'HEAD=%s lane=%s start=%s\n' "$(cat "$out/head.before")" "$lane" "$start" | tee "$out/metadata"
printf 'command: just %s' "$recipe" | tee -a "$out/metadata"
printf ' %q' "$@" | tee -a "$out/metadata"
printf '\n' | tee -a "$out/metadata"
printf 'expands to: %s\n' "$(just --dry-run "$recipe" "$@" 2>&1 | tail -n 1)" | tee -a "$out/metadata"
set +e
# flock holds the lock exactly as long as the lane runs, even if this script is
# killed, and -o keeps the descriptor out of just and the Bazel server it starts.
printf 'waiting for %s\n' "$lock"
flock -o "$lock" just "$recipe" "$@" 2>&1 | tee "$out/lane.log"
statuses=("${PIPESTATUS[@]}")
set -e
rc=${statuses[0]}
tee_rc=${statuses[1]}
manifest > "$out/inputs.after"
git rev-parse HEAD > "$out/head.after"
git status --porcelain=v1 > "$out/status.after"
changed=0
cmp -s "$out/inputs.before" "$out/inputs.after" || changed=1
cmp -s "$out/head.before" "$out/head.after" || changed=1
cmp -s "$out/status.before" "$out/status.after" || changed=1
printf 'lane_exit=%s tee_exit=%s inputs_changed=%s elapsed_seconds=%s\n' "$rc" "$tee_rc" "$changed" "$(( $(date +%s) - start ))" | tee -a "$out/metadata"
shutdown_rc=0
"${shutdown[@]}" > "$out/shutdown.log" 2>&1 || shutdown_rc=$?
printf 'shutdown: %s exit=%s\n' "${shutdown[*]}" "$shutdown_rc" | tee -a "$out/metadata"
if (( changed )); then
  diff -u "$out/inputs.before" "$out/inputs.after" > "$out/input.diff" || true
  printf 'INVALID: inputs/HEAD/status changed while lane ran\n' >&2
  exit 4
fi
if (( tee_rc )); then exit "$tee_rc"; fi
exit "$rc"
