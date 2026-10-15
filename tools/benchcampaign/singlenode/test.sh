#!/usr/bin/env bash
set -euo pipefail
here="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/singlenode"
export SCRIPTS_DIR="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/clientbench/scripts"
cd "$here"
# Scripts that act on import (VM collectors and controller drivers) are only parsed.
python3 -B -c 'import ast, sys
for path in sys.argv[1:]:
    ast.parse(open(path).read(), path)' ./*.py
for script in ./*.sh; do bash -n "$script"; done
python3 -B test_bench_runner.py
python3 -B test_seal_client_plans.py
python3 -B test_seal_design.py
python3 -B test_render_dashboard.py
python3 -B test_watch_client.py
exec python3 -B client_campaign.py self-test
