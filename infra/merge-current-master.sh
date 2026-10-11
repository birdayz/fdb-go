#!/bin/bash
# Tests a PR merged into its base as it is now, not GitHub's merge ref frozen at run creation,
# so PR jobs share a cached base. Same shape: parent 1 the base tip, parent 2 the PR head.
set -uo pipefail
[ "${GITHUB_EVENT_NAME:-}" = pull_request ] || exit 0
head=${PR_HEAD_SHA:?}
base=${PR_BASE_REF:?}
keep() {
  echo "::warning::testing GitHub's merge ref $(git rev-parse --short HEAD), not current $base: $1"
  exit 0
}
refs=("+refs/heads/$base:refs/remotes/origin/$base" "$head")
git fetch -q --no-tags --depth=200 origin "${refs[@]}" || keep "fetch failed"
# A base more than 200 commits back needs full history for both tips (a fork head included).
# A true base past the cut can still hide behind an older visible one; GitHub's ref has it too.
if ! git merge-base "origin/$base" "$head" >/dev/null 2>&1 &&
  [ "$(git rev-parse --is-shallow-repository)" = true ]; then
  git fetch -q --no-tags --unshallow origin "${refs[@]}" || keep "deepening fetch failed"
fi
git merge-base "origin/$base" "$head" >/dev/null 2>&1 || keep "no merge base with $base"
# Fail, not keep: a failed checkout may leave a half-written tree, and the merge below would
# then be a no-op that claims to test current master.
git checkout -q --detach "origin/$base" || {
  echo "::error::cannot check out current $base"
  exit 1
}
if ! git -c user.name=ci -c user.email=ci@localhost merge -q --no-ff --no-edit "$head"; then
  git merge --abort 2>/dev/null
  echo "::error::cannot merge this PR into current $base (a conflict?); rebase it"
  exit 1
fi
echo "testing PR head $(git rev-parse --short "$head") merged into current $base $(git rev-parse --short "origin/$base")"
