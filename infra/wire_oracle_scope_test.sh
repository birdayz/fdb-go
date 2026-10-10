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
export HOME=$T/home GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
mkdir -p "$HOME" "$T/bin"

fail=0
ok() { printf '  ok   %s\n' "$1"; }
bad() {
  printf '  FAIL %s\n' "$1"
  fail=1
}

# The stub accepts only the production query; $T/query_fails makes it fail.
cat >"$T/bin/bazelisk" <<'STUB'
#!/bin/bash
want="query --config=ci kind('source file', deps(//cmd/fdb-diff-oracle:fdb-diff-oracle_test + //cmd/fdb-diff-oracle:diff_oracle_bin)) + buildfiles(deps(//cmd/fdb-diff-oracle:fdb-diff-oracle_test + //cmd/fdb-diff-oracle:diff_oracle_bin)) --output=label"
[ "$*" = "$want" ] || { echo "unexpected: bazelisk $*" >&2; exit 2; }
[ -e "$SCOPETEST_DIR/query_fails" ] && exit 1
cat <<EOF
//:BUILD.bazel
//:nogo_config.json
//cmd/fdb-diff-oracle:BUILD.bazel
//cmd/fdb-diff-oracle:oracle.go
//cmd/fdb-diff-oracle:cpp/main.cpp
//pkg/fdbgo/wire:BUILD.bazel
//pkg/fdbgo/wire:reader.go
@foundationdb//:CMakeLists.txt
@rules_go//go:def.bzl
EOF
STUB
chmod +x "$T/bin/bazelisk"

R=$T/repo
mkdir -p "$R"
cd "$R" || exit 1
git init -q -b base
for f in BUILD.bazel nogo_config.json README.md MODULE.bazel .github/workflows/ci.yml \
  cmd/fdb-diff-oracle/BUILD.bazel cmd/fdb-diff-oracle/oracle.go cmd/fdb-diff-oracle/cpp/main.cpp \
  cmd/fdb-diff-oracle/testdata/fuzz/FuzzX/seed pkg/fdbgo/wire/BUILD.bazel pkg/fdbgo/wire/reader.go \
  pkg/fdbgo/wire/vtables/BUILD.bazel pkg/fdbgo/wire/vtables/v.go pkg/relational/BUILD.bazel pkg/relational/x.go; do
  mkdir -p "$(dirname "$f")"
  echo base >"$f"
done
git add -A && git commit -qm base

# $1 name; $2 event; $3 file the PR changes ("" = none); $4 expected run=; $5 output
# substring. Each PR is merged --no-ff into a base that moved on, as GitHub's merge ref is.
run_case() {
  git checkout -q -f base
  git checkout -q -B pr base
  if [ -n "$3" ]; then
    mkdir -p "$(dirname "$3")"
    echo "change" >>"$3"
    git add -A && git commit -qm pr
  fi
  git checkout -q -B moved base
  echo moved >>moved.txt && git add moved.txt && git commit -qm moved
  if [ "$2" = pull_request-head ]; then
    git checkout -q --detach pr
  else
    git merge -q --no-ff --no-edit pr
  fi
  rm -f "$T/out"
  GITHUB_EVENT_NAME=${2%-head} GITHUB_OUTPUT=$T/out SCOPETEST_DIR=$T PATH="$T/bin:$PATH" \
    bash "$SCRIPT" >"$T/log" 2>&1
  rc=$?
  got=$(cat "$T/out" 2>/dev/null)
  if [ "$rc" -eq 0 ] && [ "$got" = "run=$4" ] && grep -q "$5" "$T/log"; then
    ok "$1 (run=$4)"
  else
    bad "$1: exit $rc, wrote '$got', want run=$4 and output containing \"$5\""
    sed 's/^/       /' "$T/log"
  fi
}

echo "wire-oracle-scope:"
run_case "push to master" push "" true "push event: full run"
run_case "unrelated package" pull_request pkg/relational/x.go false "none of the 1 changed files"
run_case "root file outside the oracle" pull_request README.md false "skipped"
run_case "sibling package not depended on" pull_request pkg/fdbgo/wire/vtables/v.go false "skipped"
run_case "Go source the oracle builds" pull_request pkg/fdbgo/wire/reader.go true "is an oracle input"
run_case "C++ oracle source" pull_request cmd/fdb-diff-oracle/cpp/main.cpp true "is an oracle input"
run_case "root file the oracle reads" pull_request nogo_config.json true "is an oracle input"
run_case "fuzz seed corpus" pull_request cmd/fdb-diff-oracle/testdata/fuzz/FuzzX/seed true "in oracle package cmd/fdb-diff-oracle"
run_case "new file in an oracle package" pull_request pkg/fdbgo/wire/new.go true "in oracle package pkg/fdbgo/wire"
run_case "FDB source pin" pull_request MODULE.bazel true "pins the toolchain"
run_case "this job's workflow" pull_request .github/workflows/ci.yml true "defines this job"
run_case "PR head, not a merge" pull_request-head pkg/relational/x.go true "not a PR merge commit"
touch "$T/query_fails"
run_case "query fails" pull_request pkg/relational/x.go true "bazel query failed"

if [ "$fail" -ne 0 ]; then
  echo "FAILURES"
  exit 1
fi
echo "ALL OK"
