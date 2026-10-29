#!/usr/bin/env bash
set -euo pipefail
dir="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/multinode"
exec python3 -B "$dir/test_guarded_inputs.py"
