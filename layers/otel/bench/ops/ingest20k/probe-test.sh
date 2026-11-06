#!/usr/bin/env bash
set -Eeuo pipefail
[[ $# -ge 1 && ( $1 == inode_flags_test || $1 == inode_flags_syscall_test ) ]] || exit 2
name=$1
shift
# The repository passes its Go verbosity flag to all Bazel test rules.
for argument in "$@"; do [[ $argument == -test.v ]] || exit 2; done
: "${TEST_TMPDIR:?Bazel test scratch directory required}"
here=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
compiler=$(command -v -- cc)
compiler=$(readlink -f -- "$compiler")
"$compiler" --version > "$TEST_TMPDIR/compiler.version"
cat "$TEST_TMPDIR/compiler.version"
sha256sum -- "$compiler" "$here/inode_flags.c" "$here/$name.c"
# Unit programs inject responses or inspect pure predicates; no live probe runs.
"$compiler" -static -std=c11 -O2 -Wall -Wextra -Werror \
  "$here/$name.c" -o "$TEST_TMPDIR/$name"
"$TEST_TMPDIR/$name"
