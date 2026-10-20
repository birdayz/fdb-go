#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
p=artifacts/lifecycle-v7-build
bundle=lifecycle-v7
test ! -e "exports/$bundle.tar.gz"
grep -qx 'exit_code=0' "$p/build-status.txt"
grep -qx 'exit_code=0' "$p/run-v7-status.txt"
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binaries-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
python3 -B - <<'PY'
import json
from pathlib import Path
for comparison in ('baseline','stack'):
 for work in ('batchset50','read1000'):
  p=Path('artifacts/v7-'+comparison+'-'+work)
  assert not (p/'ABORTED.json').exists()
  c=json.loads((p/'complete.json').read_text())
  a=json.loads((p/'analysis.json').read_text())
  assert c['completed']==c['scheduled']==len(a['per_run'])==12 and a['valid_zero_error_campaign']
  assert {x['metric'] for x in a['comparisons']} >= {'process_cpu_ns_per_op','drained_cpu_ns_per_op','shutdown_process_cpu_ns_per_op'}
PY
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
paths=(artifacts/lifecycle-v7-summary artifacts/v7-baseline-batchset50 artifacts/v7-baseline-read1000 artifacts/v7-stack-batchset50 artifacts/v7-stack-read1000 "$p" bin/clientbench-v7-baseline-go bin/clientbench-v7-stack-go bin/clientbench-v7-cgo)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > "exports/$bundle.files.sha256"
sha256sum -c "exports/$bundle.files.sha256" > "exports/$bundle.prearchive-verification.log"
tar -czf "exports/$bundle.tar.gz" "${paths[@]}" "exports/$bundle.files.sha256" "exports/$bundle.prearchive-verification.log"
sha256sum "exports/$bundle.tar.gz" > "exports/$bundle.tar.gz.sha256"
cat "exports/$bundle.tar.gz.sha256"
wc -l "exports/$bundle.files.sha256"
