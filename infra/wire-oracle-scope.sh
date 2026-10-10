#!/bin/bash
# Writes run=true|false for the wire-type oracle job: a pull request runs it only when it
# changes what the oracle is built from, per Bazel. Pushes, and any failure to decide, run it.
set -uo pipefail
out=${GITHUB_OUTPUT:-/dev/stdout}
decide() {
  echo "run=$1" >>"$out"
  echo "wire-type oracle: $2"
  exit 0
}
[ "${GITHUB_EVENT_NAME:-}" = pull_request ] || decide true "${GITHUB_EVENT_NAME:-unknown} event: full run"

# A PR checkout is its merge into the base (fetch-depth: 2), so parent 1 is the base tip.
git rev-parse -q --verify HEAD^2 >/dev/null || decide true "HEAD is not a PR merge commit: full run"
changed=$(git diff --name-only --no-renames HEAD^1 HEAD) || decide true "cannot diff against the base: full run"

targets='//cmd/fdb-diff-oracle:fdb-diff-oracle_test + //cmd/fdb-diff-oracle:diff_oracle_bin'
labels=$(bazelisk query --config=ci "kind('source file', deps($targets)) + buildfiles(deps($targets))" \
  --output=label 2>/dev/null) || decide true "bazel query failed: full run"
# //pkg/a:b/c.go is pkg/a/b/c.go. External repositories change only through the pins below.
files=$(sed -n 's|^//\([^:]*\):|\1/|p' <<<"$labels" | sed 's|^/||')
pkgs=$(sed -n 's|^//\([^:][^:]*\):.*|\1|p' <<<"$labels" | sort -u)
grep -qx cmd/fdb-diff-oracle <<<"$pkgs" || decide true "query did not return the oracle package: full run"

owner() { # nearest enclosing Bazel package; "" is the root
  local d
  d=$(dirname "$1")
  while [ "$d" != . ] && [ ! -e "$d/BUILD.bazel" ] && [ ! -e "$d/BUILD" ]; do d=$(dirname "$d"); done
  [ "$d" = . ] && d=
  echo "$d"
}

n=0
while IFS= read -r f; do
  [ -n "$f" ] || continue
  n=$((n + 1))
  case "$f" in
  MODULE.bazel | MODULE.bazel.lock | go.mod | go.sum | .bazelrc | .bazelversion | \
    .github/workflows/ci.yml | infra/wire-oracle-scope.sh)
    decide true "$f pins the toolchain or defines this job: full run"
    ;;
  esac
  grep -qxF "$f" <<<"$files" && decide true "$f is an oracle input: full run"
  # Plain `go test` also reads non-Bazel files of its package (the fuzz seed corpora).
  p=$(owner "$f")
  [ -n "$p" ] && grep -qxF "$p" <<<"$pkgs" && decide true "$f is in oracle package $p: full run"
done <<<"$changed"
decide false "none of the $n changed files reaches the oracle: skipped"
