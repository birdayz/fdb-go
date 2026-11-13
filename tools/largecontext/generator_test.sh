#!/usr/bin/env bash
set -euo pipefail
cd "${TEST_SRCDIR}/${TEST_WORKSPACE}"
export PYTHONUTF8=1
python3 tools/largecontext/generator_test.py
