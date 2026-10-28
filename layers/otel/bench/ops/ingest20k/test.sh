#!/usr/bin/env bash
set -euo pipefail
[[ $# -ge 1 ]] || { echo 'Expected one explicit test module' >&2; exit 2; }
module=$1
shift
case "$module" in
    test_policy|test_runtime|test_lifecycle|test_shell) ;;
    *) echo 'Unknown test module' >&2; exit 2 ;;
esac
# The repository passes Go verbosity to every Bazel test rule.
for argument in "$@"; do
    [[ $argument == -test.v ]] || { echo 'Unknown test argument' >&2; exit 2; }
done
here="${TEST_SRCDIR}/${TEST_WORKSPACE}/layers/otel/bench/ops/ingest20k"
cd "$here"
python3 -B -c 'import ast, sys
for path in sys.argv[1:]:
    ast.parse(open(path).read(), path)' ./*.py
for script in ./*.sh; do bash -n "$script"; done
exec python3 -B - "$module" <<'PY'
import importlib
import sys
import unittest

name = sys.argv[1]
module = importlib.import_module(name)
suite = unittest.defaultTestLoader.loadTestsFromModule(module)
count = suite.countTestCases()
print(f"{name}: discovered {count} tests", flush=True)
if count <= 0:
    print(f"{name}: rejected empty test population", file=sys.stderr)
    sys.exit(1)
result = unittest.TextTestRunner(stream=sys.stdout, verbosity=2).run(suite)
print(f"{name}: ran {result.testsRun}/{count}; failures={len(result.failures)}; "
      f"errors={len(result.errors)}; skipped={len(result.skipped)}", flush=True)
sys.exit(0 if result.wasSuccessful() and result.testsRun == count and not result.skipped else 1)
PY
