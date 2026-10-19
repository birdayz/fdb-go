#!/usr/bin/env bash
set -euo pipefail
scripts="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/clientbench/scripts"
python3 -B "$scripts/test_campaign.py"
python3 -B "$scripts/test_wirediff.py"
python3 -B "$scripts/test_gcalign.py"
python3 -B "$scripts/test_wiregolden.py"
exec python3 -B "$scripts/test_matrix_plan.py"
