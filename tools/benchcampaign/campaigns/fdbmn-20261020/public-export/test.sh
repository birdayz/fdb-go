#!/usr/bin/env bash
set -euo pipefail
dir="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/campaigns/fdbmn-20261020/public-export"
exec python3 -B -m unittest discover -s "$dir" -p 'test_*.py'
