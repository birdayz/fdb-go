#!/bin/bash
# Drives infra/merge-current-master.sh against a real origin and a depth-1 checkout of a stale
# GitHub merge ref: the job must test the PR merged into the base as it is now.
#
# Run: bash infra/merge_current_master_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."
SCRIPT=$(pwd)/infra/merge-current-master.sh

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export HOME=$T/home GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir -p "$HOME"

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
  printf '  FAIL %s\n' "$1"
  fail=1
}

# origin: master m0, PR p1 off m0 adding pr.txt, GitHub's merge ref of p1 into m0, then master
# moves on by $1 commits (each appending to master.txt).
setup() {
  cd "$T" || exit 1
  rm -rf "$T/src" "$T/origin.git" "$T/work"
  git init -q -b master "$T/src"
  (
    cd "$T/src" || exit 1
    echo base >shared.txt && echo 0 >master.txt && git add -A && git commit -qm m0
    git checkout -q -b pr && echo pr >pr.txt
    [ -n "${2:-}" ] && echo pr-side >shared.txt
    git add -A && git commit -qm p1
    git checkout -q --detach master && git merge -q --no-ff --no-edit pr && git update-ref refs/pull/1/merge HEAD
    git checkout -q master
    for i in $(seq 1 "$1"); do echo "$i" >>master.txt && git commit -qam "m$i"; done
    [ -z "${2:-}" ] || { echo master-side >shared.txt && git commit -qam conflict; }
  )
  git clone -q --bare "$T/src" "$T/origin.git"
  git -C "$T/src" push -q "$T/origin.git" refs/pull/1/merge:refs/pull/1/merge
  git -C "$T/origin.git" config uploadpack.allowAnySHA1InWant true
  # What actions/checkout does: a depth-1 fetch of the merge ref, detached.
  git init -q "$T/work" && cd "$T/work" || exit 1
  git remote add origin "file://$T/origin.git"
  git fetch -q --no-tags --depth=1 origin +refs/pull/1/merge:refs/remotes/pull/1/merge
  git checkout -q --detach refs/remotes/pull/1/merge
  STALE=$(git rev-parse HEAD)
  PR=$(git -C "$T/src" rev-parse pr)
  MASTER=$(git -C "$T/src" rev-parse master)
}

# $1 name; $2 event; $3 expected exit; $4 expected HEAD^1 ("stale" = untouched, "any"); $5 output.
check() {
  GITHUB_EVENT_NAME=$2 PR_HEAD_SHA=${HEAD_SHA:-$PR} PR_BASE_REF=master bash "$SCRIPT" >"$T/log" 2>&1
  rc=$?
  if [ "$4" = any ]; then
    good=yes
  elif [ "$4" = stale ]; then
    [ "$(git rev-parse HEAD)" = "$STALE" ] && good=yes || good=no
  else
    [ "$(git rev-parse HEAD^1)" = "$4" ] && [ "$(git rev-parse HEAD^2)" = "$PR" ] &&
      [ -f pr.txt ] && [ "$(tail -1 master.txt)" = "$(git -C "$T/src" show master:master.txt | tail -1)" ] &&
      good=yes || good=no
  fi
  said=yes
  [ -z "$5" ] || grep -qF -- "$5" "$T/log" || said=no
  if [ "$rc" -eq "$3" ] && [ "$good" = yes ] && [ "$said" = yes ]; then
    ok "$1"
  else
    bad "$1: exit $rc (want $3), HEAD ok=$good, output:"
    sed 's/^/       /' "$T/log"
  fi
}

echo "merge-current-master:"
setup 3
check "push keeps its own commit" push 0 stale ""
setup 3
check "stale merge ref is re-merged onto current master" pull_request 0 "$MASTER" "merged into current master"
setup 250
check "base more than 200 commits ahead deepens history" pull_request 0 "$MASTER" "merged into current master"
setup 3 conflict
check "conflict with current master fails the job" pull_request 1 any "conflicts with current master"
[ "$(git status --porcelain | grep -c '^UU')" -eq 0 ] && ok "conflict leaves no half-merge" || bad "conflict left a half-merge"
setup 3
HEAD_SHA=0123456789abcdef0123456789abcdef01234567 check "unfetchable PR head keeps the merge ref" pull_request 0 stale "testing GitHub's merge ref"

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
