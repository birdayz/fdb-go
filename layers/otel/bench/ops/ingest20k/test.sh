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

success = True
for name in ("test_policy", "test_runtime", "test_lifecycle", "test_shell"):
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
    success = success and result.wasSuccessful() and result.testsRun == count and not result.skipped
sys.exit(0 if success else 1)
PY
