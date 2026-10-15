#!/usr/bin/env bash
set -euo pipefail
dir="${TEST_SRCDIR}/${TEST_WORKSPACE}/tools/benchcampaign/multinode"
python3 -B "$dir/test_placementprobe.py"
python3 -B "$dir/test_placement_command.py"
python3 -B "$dir/test_client_campaign_mn.py"
python3 -B "$dir/test_bench_runner_mn.py"
python3 -B "$dir/test_node_sampler.py"
python3 -B "$dir/test_reconcile_telemetry.py"
python3 -B "$dir/test_build_reconcile_input.py"
exec python3 -B "$dir/test_multinode.py"
