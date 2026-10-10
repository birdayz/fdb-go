#!/bin/bash
# Writes run=true|false for the wire-type oracle job: a pull request runs it only when it
# changes what the oracle is built from, per Bazel. Pushes, and any failure to decide, run it.
set -uo pipefail
out=${GITHUB_OUTPUT:-/dev/stdout}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
decide() {
  echo "run=$1" >>"$out"
  echo "wire-type oracle: $2"
  [ "$1" = true ] || echo "::notice::wire-type oracle skipped: $2"
  exit 0
}
[ "${GITHUB_EVENT_NAME:-}" = pull_request ] || decide true "${GITHUB_EVENT_NAME:-unknown} event: full run"

# A PR checkout is its merge into the base (fetch-depth: 2), so parent 1 is the base tip.
git rev-parse -q --verify HEAD^2 >/dev/null || decide true "HEAD is not a PR merge commit: full run"
# -z: unquoted paths; --no-renames: a moved-away oracle input still lists its old path.
git diff -z --name-only --no-renames HEAD^1 HEAD >"$tmp/changed" ||
  decide true "cannot diff against the base: full run"

# Files outside the query's reach that change the toolchain, Go module layout or external
# repositories (MODULE.bazel's patches and root repository rules).
while IFS= read -r -d '' f; do
  case "$f" in
  .github/workflows/ci.yml | infra/wire-oracle-scope.sh)
    decide true "$f defines this job: full run"
    ;;
  MODULE.bazel | MODULE.bazel.lock | *.MODULE.bazel | REPO.bazel | patches/* | go.mod | go.sum | \
    */go.mod | go.work | go.work.sum | .bazelrc | user.bazelrc | .bazelversion | .bazeliskrc | tools/bazel)
    decide true "$f pins the toolchain, module layout or external repositories: full run"
    ;;
  esac
  [[ $f != */* && $f == *.bzl ]] && decide true "$f defines a repository rule: full run"
done <"$tmp/changed"

targets='//cmd/fdb-diff-oracle:fdb-diff-oracle_test + //cmd/fdb-diff-oracle:diff_oracle_bin'
if ! bazelisk query --config=ci "kind('source file', deps($targets)) + buildfiles(deps($targets))" \
  --output=label >"$tmp/labels" 2>"$tmp/err"; then
  tail -n 20 "$tmp/err"
  decide true "bazel query failed: full run"
fi
# //pkg/a:b/c.go is pkg/a/b/c.go; external labels are covered by the pins above.
sed -n 's|^//\([^:]*\):|\1/|p' "$tmp/labels" | sed 's|^/||' >"$tmp/files"
sed -n 's|^//\([^:][^:]*\):.*|\1|p' "$tmp/labels" | sort -u >"$tmp/pkgs"
# The oracle, its Go wire types and its C++ build: missing any means the query was misread.
for p in cmd/fdb-diff-oracle pkg/fdbgo/wire/types cmd/fdb-schema-extract; do
  grep -qxF -e "$p" "$tmp/pkgs" || decide true "query did not return oracle package $p: full run"
done

owner() { # nearest enclosing Bazel package; "" is the root
  local d
  d=$(dirname -- "$1")
  while [ "$d" != . ] && [ ! -f "$d/BUILD.bazel" ] && [ ! -f "$d/BUILD" ]; do d=$(dirname -- "$d"); done
  [ "$d" = . ] && d=
  echo "$d"
}

n=0
while IFS= read -r -d '' f; do
  n=$((n + 1))
  grep -qxF -e "$f" "$tmp/files" && decide true "$f is an oracle input: full run"
  # Plain `go test` also reads non-Bazel files of its package (the fuzz seed corpora).
  p=$(owner "$f")
  [ -n "$p" ] && grep -qxF -e "$p" "$tmp/pkgs" && decide true "$f is in oracle package $p: full run"
done <"$tmp/changed"
decide false "none of the $n changed files reaches the oracle"
