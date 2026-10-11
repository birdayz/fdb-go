#!/bin/bash
# Tests a PR merged into its base as it is now, not GitHub's merge ref from its last push (reopens
# and re-runs reuse it), so PR jobs share a cached base. Parent 1 the base tip, parent 2 the head.
set -uo pipefail
[ "${GITHUB_EVENT_NAME:-}" = pull_request ] || exit 0
head=${PR_HEAD_SHA:?}
base=${PR_BASE_REF:?}
keep() {
  echo "::warning::testing GitHub's merge ref $(git rev-parse --short HEAD), not current $base: $1"
  exit 0
}
refs=("+refs/heads/$base:refs/remotes/origin/$base" "$head")
# Bounded: git has no network timeout, and a stalled fetch would hold the runner.
fetch() { timeout 120 git -c gc.auto=0 fetch -q --no-tags "$@" origin "${refs[@]}"; }
fetch --depth=200 || keep "fetch failed"
# A base more than 200 commits back needs full history for both tips (a fork head included).
# Known gap: a true base past the cut can hide behind an older visible one (>200 commits behind).
if ! git merge-base "origin/$base" "$head" >/dev/null 2>&1 &&
  [ "$(git rev-parse --is-shallow-repository)" = true ]; then
  fetch --unshallow || keep "deepening fetch failed"
fi
git merge-base "origin/$base" "$head" >/dev/null 2>&1 || keep "no merge base with $base"
# Fail, not keep: a failed checkout may leave a half-written tree, and the merge below would
# then be a no-op that claims to test current master.
git checkout -q --detach "origin/$base" || {
  echo "::error::cannot check out current $base"
  exit 1
}
if ! git -c user.name=ci -c user.email=ci@localhost merge -q --no-ff --no-edit "$head"; then
  git diff --name-only --diff-filter=U | sed 's/^/unmerged: /'
  git merge --abort 2>/dev/null
  echo "::error::cannot merge this PR into current $base (a conflict?); rebase it"
  exit 1
fi
echo "testing PR head $(git rev-parse --short "$head") merged into current $base $(git rev-parse --short "origin/$base")"
