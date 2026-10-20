#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
p=artifacts/stack-r14-build
bundle=stack-r14
test ! -e exports/$bundle.tar.gz
grep -qx exit_code=0 "$p/profile-v2-status.txt"
grep -qx exit_code=0 artifacts/stack-r14-analysis/status.txt
python3 -B - <<'PY'
import json
from pathlib import Path
p=Path('artifacts/stack-r14')
assert json.loads((p/'complete.json').read_text())['completed']==6
rows=[]
for folder in sorted(p.iterdir()):
    if not folder.is_dir():continue
    s=json.loads((folder/'summary.json').read_text());n=s['completed'];a=s['stats_before'];b=s['stats_after']
    assert s['success'] and s['retries']==s['warmup_retries']==0
    rows.append(dict(run=folder.name,iterations=n,warmup=s['warmup_completed'],go_alloc_bytes_per_op=(b['go_total_alloc_bytes']-a['go_total_alloc_bytes'])/n,go_mallocs_per_op=(b['go_mallocs']-a['go_mallocs'])/n,go_gc_cycles=b['go_gc_cycles']-a['go_gc_cycles']))
assert len(rows)==6
(p/'attribution-window-memory.json').write_text(json.dumps(dict(scope='Instrumented runs, no timing comparison. Measured-window GoMemStats only; native C++ heap excluded. CPUpprof measured loop; heap sampled lifetime allocation and postforcedGC liveheap; perf wholeprocess includes identity startup/fixture/warmup/validation/shutdown.',rows=rows),indent=2)+'\n')
quality=json.loads(Path('artifacts/stack-r14-analysis/sample-split-and-quality.json').read_text())
assert set(quality)=={'new','cgo'}
assert all(v['sample_count']>0 for v in quality.values())
folds=list(Path('artifacts/stack-r14-analysis').glob('*-folded/fold-validation.json'))
assert len(folds)==4
assert all(json.loads(f.read_text())['samples']>0 for f in folds)
PY
sha256sum -c "$p/input-v2.sha256" > "$p/archive-profile-input-check.log"
sha256sum -c "$p/analysis-input.sha256" > "$p/archive-analysis-input-check.log"
sha256sum -c artifacts/stack-r11v2-build/binaries.sha256 > "$p/archive-binary-check.log"
sha256sum -c artifacts/stack-r11v2-build/runtime.sha256 > "$p/archive-runtime-check.log"
ps -eo pid,ppid,comm,args > "$p/archive-processes.txt"
paths=("$p" artifacts/stack-r14 artifacts/stack-r14-analysis artifacts/stack-r11v2-build bin/candidate-stack-r11v2-new bin/candidate-v6-cgo scripts/astra-perf-run.py scripts/astra-perf-analyze.py)
for file in plan.json schedule.json binaries.json complete.json;do paths+=("artifacts/stack-r11v2-read1000/$file");done
find "${paths[@]}" -type f -print0 | sort -z | xargs -0 sha256sum > exports/$bundle.files.sha256
sha256sum -c exports/$bundle.files.sha256 > exports/$bundle.prearchive-verification.log
tar -czf exports/$bundle.tar.gz "${paths[@]}" exports/$bundle.files.sha256 exports/$bundle.prearchive-verification.log
sha256sum exports/$bundle.tar.gz > exports/$bundle.tar.gz.sha256
cat exports/$bundle.tar.gz.sha256
wc -l exports/$bundle.files.sha256
