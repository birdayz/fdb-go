#!/usr/bin/env bash
set -euo pipefail
dir="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/campaigns/fdbmn-rfc282/fences"
exec python3 -B -m unittest discover -s "$dir" -p 'test_*.py'
