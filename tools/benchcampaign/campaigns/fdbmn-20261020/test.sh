#!/usr/bin/env bash
# Offline controls of the campaign's frozen analysis adapter and phase controller.
# The frozen tests write temporary directories beside themselves, so they run
# from a writable copy; the copied bytes are the pinned ones.
set -euo pipefail
src="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/campaigns/fdbmn-20261020/execution"
dir="${TEST_TMPDIR}/execution"
mkdir -p "$dir/controller-tests"
for f in analyze_campaign.py test_analyze_campaign.py run_phase.py remote.py; do cp -L "$src/$f" "$dir/$f"; done
cp -L "$src/controller-tests/test_run_phase.py" "$dir/controller-tests/test_run_phase.py"
python3 -B "$dir/test_analyze_campaign.py"
exec python3 -B "$dir/controller-tests/test_run_phase.py"
