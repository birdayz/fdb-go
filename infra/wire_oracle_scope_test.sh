#!/bin/bash
# Drives infra/wire-oracle-scope.sh against a real git fixture of PR merge commits and a
# stubbed `bazelisk query`: skip only when no change reaches the oracle, else run it.
#
# Run: bash infra/wire_oracle_scope_test.sh
set -uo pipefail
cd "$(dirname "$0")/.."
SCRIPT=$(pwd)/infra/wire-oracle-scope.sh

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
export HOME=$T/home GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir -p "$HOME" "$T/bin"

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
  printf '  FAIL %s\n' "$1"
  fail=1
}

# The stub accepts only the production query. $T/query_mode: ok, fail, foreign (labels in
# a form the script cannot read, e.g. --consistent_labels' @@//), or partial (an edge lost).
cat >"$T/bin/bazelisk" <<'STUB'
#!/bin/bash
t='//cmd/fdb-diff-oracle:fdb-diff-oracle_test + //cmd/fdb-diff-oracle:diff_oracle_bin'
[ "$#" -eq 4 ] && [ "$1" = query ] && [ "$2" = --config=ci ] && [ "$4" = --output=label ] &&
  [ "$3" = "kind('source file', deps($t)) + buildfiles(deps($t))" ] ||
  { echo "unexpected: bazelisk $*" >&2; exit 2; }
case "$(cat "$SCOPETEST_DIR/query_mode")" in
fail) echo "ERROR: query exploded" >&2; exit 1 ;;
foreign) echo "@@//cmd/fdb-diff-oracle:oracle.go"; echo "@foundationdb//:CMakeLists.txt"; exit 0 ;;
partial) skip=cmd/fdb-schema-extract ;;
*) skip=none ;;
esac
grep -v "^//$skip:" <<EOF
//:BUILD.bazel
//:nogo_config.json
//cmd/fdb-diff-oracle:BUILD.bazel
//cmd/fdb-diff-oracle:oracle.go
//cmd/fdb-diff-oracle:cpp/main.cpp
//pkg/fdbgo/wire:BUILD.bazel
//pkg/fdbgo/wire:reader.go
//pkg/fdbgo/wire/types:BUILD.bazel
//pkg/fdbgo/wire/types:types.go
//cmd/fdb-schema-extract:BUILD.bazel
//cmd/fdb-schema-extract:fdb_cpp_build.bzl
@foundationdb//:CMakeLists.txt
@rules_go//go:def.bzl
EOF
STUB
chmod +x "$T/bin/bazelisk"

R=$T/repo
mkdir -p "$R"
cd "$R" || exit 1
git init -q -b base
for f in BUILD.bazel nogo_config.json README.md MODULE.bazel sift_repo.bzl patches/BUILD.bazel \
  patches/x.patch .github/workflows/ci.yml cmd/fdb-diff-oracle/BUILD.bazel cmd/fdb-diff-oracle/oracle.go \
  cmd/fdb-diff-oracle/cpp/main.cpp cmd/fdb-diff-oracle/testdata/fuzz/FuzzX/seed \
  pkg/fdbgo/wire/BUILD.bazel pkg/fdbgo/wire/reader.go pkg/fdbgo/wire/vtables/BUILD.bazel \
  pkg/fdbgo/wire/vtables/v.go pkg/fdbgo/wire/types/BUILD.bazel pkg/fdbgo/wire/types/types.go \
  cmd/fdb-schema-extract/BUILD.bazel cmd/fdb-schema-extract/fdb_cpp_build.bzl \
  pkg/relational/BUILD.bazel pkg/relational/x.go; do
  mkdir -p "$(dirname "$f")"
  echo "base $f" >"$f"
done
git add -A && git commit -qm base

# $1 name; $2 checkout: merge (GitHub's merge ref), head (the PR head), or push; $3 the PR's
# changes, space-separated: a path to append to, rm:PATH, or mv:FROM:TO; $4 query mode; $5 expected
# run=; $6 output substring. The base moves on first, as it does under a real PR.
run_case() {
  git checkout -q -f base
  git checkout -q -B pr base
  for c in $3; do
    case "$c" in
    mv:*)
      IFS=: read -r _ from to <<<"$c"
      mkdir -p "$(dirname "$to")"
      git mv "$from" "$to"
      ;;
    rm:*) git rm -q -- "${c#rm:}" ;;
    *)
      mkdir -p "$(dirname "$c")"
      echo change >>"$c"
      git add -- "$c"
      ;;
    esac
  done
  git commit -qm pr
  git checkout -q -B moved base
  echo moved >>moved.txt && git add moved.txt && git commit -qm moved
  event=pull_request
  case "$2" in
  merge) git merge -q --no-ff --no-edit pr ;;
  head) git checkout -q --detach pr ;;
  push) event=push ;;
  esac
  echo "$4" >"$T/query_mode"
  rm -f "$T/out"
  GITHUB_EVENT_NAME=$event GITHUB_OUTPUT=$T/out SCOPETEST_DIR=$T PATH="$T/bin:$PATH" \
    bash "$SCRIPT" >"$T/log" 2>&1
  rc=$?
  got=$(cat "$T/out" 2>/dev/null)
  if [ "$rc" -eq 0 ] && [ "$got" = "run=$5" ] && grep -qF -- "$6" "$T/log"; then
    ok "$1 (run=$5)"
  else
    bad "$1: exit $rc, wrote '$got', want run=$5 and output containing \"$6\""
    sed 's/^/       /' "$T/log"
  fi
}

echo "wire-oracle-scope:"
run_case "push to master" push pkg/relational/x.go ok true "push event: full run"
run_case "unrelated package" merge pkg/relational/x.go ok false "none of the 1 changed files"
run_case "root file outside the oracle" merge README.md ok false "::notice::wire-type oracle skipped"
run_case "several unrelated files" merge "README.md pkg/relational/x.go moved2.txt" ok false "none of the 3 changed files"
run_case "sibling package not depended on" merge pkg/fdbgo/wire/vtables/v.go ok false "skipped"
# README.md sorts before pkg/, so the relevant file is not the first one seen.
run_case "oracle input after an unrelated file" merge "README.md pkg/fdbgo/wire/reader.go" ok true "pkg/fdbgo/wire/reader.go is an oracle input"
run_case "C++ oracle source" merge cmd/fdb-diff-oracle/cpp/main.cpp ok true "is an oracle input"
run_case "root file the oracle reads" merge nogo_config.json ok true "is an oracle input"
run_case "fuzz seed corpus" merge cmd/fdb-diff-oracle/testdata/fuzz/FuzzX/seed ok true "in oracle package cmd/fdb-diff-oracle"
run_case "new file in an oracle package" merge pkg/fdbgo/wire/new.go ok true "in oracle package pkg/fdbgo/wire"
run_case "non-ASCII name in an oracle package" merge "pkg/fdbgo/wire/ä.go" ok true "in oracle package pkg/fdbgo/wire"
run_case "oracle input moved out" merge mv:pkg/fdbgo/wire/reader.go:pkg/relational/reader.go ok true "pkg/fdbgo/wire/reader.go is an oracle input"
run_case "root BUILD the oracle loads" merge BUILD.bazel ok true "BUILD.bazel is an oracle input"
run_case "oracle input deleted" merge rm:pkg/fdbgo/wire/reader.go ok true "pkg/fdbgo/wire/reader.go is an oracle input"
run_case "C++ build rule" merge cmd/fdb-schema-extract/fdb_cpp_build.bzl ok true "is an oracle input"
run_case "FDB source pin" merge MODULE.bazel ok true "pins the toolchain, module layout"
run_case "module patch" merge patches/x.patch ok true "pins the toolchain, module layout"
run_case "nested module splits the oracle's" merge pkg/go.mod ok true "pins the toolchain, module layout"
run_case "pin decided before the query" merge MODULE.bazel fail true "pins the toolchain, module layout"
run_case "root repository rule" merge sift_repo.bzl ok true "defines a repository rule"
run_case "this job's workflow" merge .github/workflows/ci.yml ok true "defines this job"
run_case "PR head, not a merge" head pkg/relational/x.go ok true "not a PR merge commit"
run_case "query fails, and says why" merge pkg/relational/x.go fail true "query exploded"
run_case "query output unreadable" merge pkg/relational/x.go foreign true "did not return oracle package cmd/fdb-diff-oracle"
run_case "query lost the C++ edge" merge pkg/relational/x.go partial true "did not return oracle package cmd/fdb-schema-extract"

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
