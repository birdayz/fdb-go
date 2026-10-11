#!/usr/bin/env bash
set -euo pipefail
exec python3 -B "${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/clientbench/scripts/test_campaign.py"
