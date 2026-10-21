#!/usr/bin/env bash
set -euo pipefail
exec python3 -B "${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/campaigns/rfc-264/local/test_quick_local.py"
