#!/bin/bash
# Tests a PR merged into its base branch as it is NOW, not GitHub's merge ref frozen when the
# run was created (re-runs keep it for days), so PR jobs share a base whose results are cached.
# Parent 1 is the base tip and parent 2 the PR head, the shape of GitHub's merge commit.
set -uo pipefail
[ "${GITHUB_EVENT_NAME:-}" = pull_request ] || exit 0
head=${PR_HEAD_SHA:?}
base=${PR_BASE_REF:?}
keep() {
  echo "::warning::testing GitHub's merge ref $(git rev-parse --short HEAD), not current $base: $1"
  exit 0
}
git fetch -q --no-tags --depth=200 origin "+refs/heads/$base:refs/remotes/origin/$base" "$head" ||
  keep "fetch failed"
git merge-base "origin/$base" "$head" >/dev/null 2>&1 ||
  { git fetch -q --no-tags --unshallow origin 2>/dev/null || git fetch -q --no-tags origin; } ||
  keep "fetch failed"
git merge-base "origin/$base" "$head" >/dev/null 2>&1 || keep "no merge base with $base"
git checkout -q --detach "origin/$base"
if ! git -c user.name=ci -c user.email=ci@localhost merge -q --no-ff --no-edit "$head"; then
  git merge --abort 2>/dev/null
  echo "::error::this PR conflicts with current $base; rebase it"
  exit 1
fi
echo "testing PR head $(git rev-parse --short "$head") merged into current $base $(git rev-parse --short "origin/$base")"
