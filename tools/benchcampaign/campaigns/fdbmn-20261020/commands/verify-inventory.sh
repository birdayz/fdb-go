# READ ONLY. Use a fresh output directory each time; exit != 0 means teardown is NOT complete.
set -euo pipefail
python3 -B ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/verify_inventory.py --inventory ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/inventory --output-dir ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/work/verify-post-teardown
