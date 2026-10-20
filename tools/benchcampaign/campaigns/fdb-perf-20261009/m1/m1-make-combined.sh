#!/usr/bin/env bash
# Construct the M1 combined NEW arm as a local branch: START (a full SHA that descends from
# BASE, e.g. (A)'s head) plus cherry-picks applied in order. Any conflict or empty pick aborts loudly.
# Usage: m1-make-combined.sh BRANCH BASE_FULLSHA START_FULLSHA PICK [PICK ...]
#   PICK = full SHA of one commit, or FULLSHA..FULLSHA for a range (oldest excluded, as in git).
# Prints the combined full SHA. Never touches other agents' worktrees; uses worktrees/m1-combine.
set -Eeuo pipefail
br=${1:?branch}; base=${2:?base}; start=${3:?start}; shift 3
[[ $# -ge 1 ]] || { echo "need >=1 PICK" >&2; exit 2; }
repo=${FDB_GO_WORKTREES}/fdb-go
wt=${FDB_PERF_HOME}/worktrees/m1-combine
full() { [[ $1 =~ ^[0-9a-f]{40}$ ]] || { echo "not a full SHA: $1" >&2; exit 2; }; }
full "$base"; full "$start"
git -C "$repo" merge-base --is-ancestor "$base" "$start" || { echo "START does not descend from BASE" >&2; exit 1; }
for p in "$@"; do
  if [[ $p == *..* ]]; then full "${p%%..*}"; full "${p##*..}"; else full "$p"; fi
done
if [[ -d $wt ]]; then git -C "$repo" worktree remove --force "$wt"; fi
git -C "$repo" worktree add --detach "$wt" "$start" >/dev/null
# Fixed committer identity/date: the same inputs always give the same combined SHA.
export GIT_COMMITTER_NAME="m1-harness" GIT_COMMITTER_EMAIL="m1-harness@localhost" GIT_COMMITTER_DATE="2026-01-01T00:00:00Z"
for p in "$@"; do
  # Hooks are not run by cherry-pick; validation of NEW is the owners' CI, not this script.
  if ! git -C "$wt" cherry-pick "$p"; then
    echo "CONFLICT/EMPTY picking $p onto $(git -C "$wt" rev-parse HEAD):" >&2
    git -C "$wt" status --short >&2 || true
    git -C "$wt" cherry-pick --abort 2>/dev/null || true
    exit 1
  fi
done
head=$(git -C "$wt" rev-parse HEAD)
git -C "$repo" merge-base --is-ancestor "$base" "$head"
old=$(git -C "$repo" rev-parse -q --verify "refs/heads/$br" || true)
if [[ -n $old && $old != "$head" && ${M1_FORCE:-} != 1 ]]; then
  echo "branch $br exists at $old; set M1_FORCE=1 to move it to $head" >&2; exit 1
fi
git -C "$repo" update-ref "refs/heads/$br" "$head"
{ echo "branch=$br head=$head base=$base start=$start picks=$*"
  git -C "$repo" log --format='%H %s' "$base..$head"; } | tee "${FDB_PERF_HOME}/results/m1/combined-$head.txt"
echo "$head"
