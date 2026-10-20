#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
p=artifacts/pr6-r12b-build
bundle=pr6-r12b
test ! -e exports/$bundle.tar.gz
grep -qx exit_code=0 "$p/run-r12b-status.txt"
grep -qx exit_code=0 "$p/analysis-v2-status.txt"
python3 -B - <<'PY'
import json
from pathlib import Path
p=Path('artifacts')
assert json.loads((p/'pr6-r12b-profile/complete.json').read_text())['completed']==4
for name in ['pr6-r12-batchset50','pr6-r12b-batchset50']:
    c=json.loads((p/name/'complete.json').read_text());assert c['completed']==c['scheduled']==18
out=p/'pr6-r12b-profile-analysis'
assert len(list(out.glob('*-top40.txt')))==10
assert all(f.stat().st_size>0 for f in out.glob('*-top40.txt'))
PY
sha256sum -c "$p/measurement-input.sha256" > "$p/archive-measurement-input-check.log"
sha256sum -c "$p/attribution-input.sha256" > "$p/archive-attribution-input-check.log"
sha256sum -c "$p/pooled-v2-input.sha256" > "$p/archive-pooled-v2-input-check.log"
sha256sum -c "$p/binaries.sha256" > "$p/archive-binary-check.log"
sha256sum -c "$p/runtime.sha256" > "$p/archive-runtime-check.log"
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
paths=("$p" artifacts/pr6-r12b-batchset50 artifacts/pr6-r12b-profile artifacts/pr6-r12b-profile-analysis artifacts/pr6-r12b-pooled artifacts/pr6-r12-batchset50 artifacts/pr6-r12-build bin/candidate-pr5-r10b-old bin/candidate-pr6-r12-new bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > exports/$bundle.files.sha256
sha256sum -c exports/$bundle.files.sha256 > exports/$bundle.prearchive-verification.log
tar -czf exports/$bundle.tar.gz "${paths[@]}" exports/$bundle.files.sha256 exports/$bundle.prearchive-verification.log
sha256sum exports/$bundle.tar.gz > exports/$bundle.tar.gz.sha256
cat exports/$bundle.tar.gz.sha256
wc -l exports/$bundle.files.sha256
