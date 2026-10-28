#!/usr/bin/env bash
set -euo pipefail
here="${TEST_SRCDIR}/${TEST_WORKSPACE}/layers/otel/bench/ops/ingest20k"
cd "$here"
python3 -B -c 'import ast, sys
for path in sys.argv[1:]:
    ast.parse(open(path).read(), path)' ./*.py
for script in ./*.sh; do bash -n "$script"; done
exec python3 -B - <<'PY'
import importlib
import sys
import unittest

module = importlib.import_module("test_policy")
suite = unittest.defaultTestLoader.loadTestsFromModule(module)
count = suite.countTestCases()
print(f"test_policy: discovered {count} tests", flush=True)
if count <= 0:
    print("test_policy: rejected empty test population", file=sys.stderr)
    sys.exit(1)
result = unittest.TextTestRunner(stream=sys.stdout, verbosity=2).run(suite)
print(f"test_policy: ran {result.testsRun}/{count}; failures={len(result.failures)}; "
      f"errors={len(result.errors)}; skipped={len(result.skipped)}", flush=True)
sys.exit(0 if result.wasSuccessful() and result.testsRun == count and not result.skipped else 1)
PY
