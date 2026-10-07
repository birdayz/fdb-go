#!/usr/bin/env bash
# Runs the sqldriver_test binary with the arguments sqldriver_fast_test passes
# (the heavy sweeps skipped). See SQLDRIVER_HEAVY_TESTS in BUILD.bazel.
#
# A go_test runs in its package directory; tests here read testdata/ by a
# relative path, so the binary is run from the same directory of the runfiles
# tree.
set -euo pipefail
bin="$PWD/$1"
shift
cd "$TEST_SRCDIR/$TEST_WORKSPACE/pkg/relational/sqldriver"
exec "$bin" "$@"
