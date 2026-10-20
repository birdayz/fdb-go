#!/usr/bin/env bash
set -euo pipefail
dir="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/campaigns/fdbmn-20261020/public-export"
exec python3 -B "$dir/test_export_public_results.py"
