#!/usr/bin/env python3
"""Add explicit frozen-window caveats and pair-adjacent GC observations."""
import csv
import json
from pathlib import Path
import sys

out,round = Path(sys.argv[1]),sys.argv[2]
data=json.loads((out/(round.lower()+'-detailed.json')).read_text())
rows=data['per_run'];workload=rows[0]['workload']
gc={arm:sum(r['go_gc_cycles'] for r in rows if r['arm']==arm) for arm in ['old','new','cgo']}
text=['\n## Frozen-window CPU limitation (review disclosure)\n\n'
      'CPU here is only measured-window getrusage, NOT complete-lifecycle or steady-state per-transaction CPU. '
      'The Apple Go binding releases native transactions/futures through Go GC finalizers '
      '(7.3.77 bindings/go/src/fdb/database.go:102-106,futures.go:93); transaction destruction then queues native network-thread work '
      '(bindings/c/fdb_c.cpp:921-926;fdbclient/ThreadSafeTransaction.cpp:402-406). Warmup cleanup can enter the window; '
      'late measured-transaction cleanup can fall after it. No finalizer/network drain brackets this frozen V6 window. '
      'The size/direction of that unequal deferred-cleanup effect is unmeasured. These CPU ratios are provisional window observations, '
      'not published native full-cost/capacity claims; a separate lifecycle-accounting campaign is required. '
      'GC counts are Go-runtime cycles, not a proof that all C++ cleanup completed.\n\n'
      '|Ratio|Window CPU paired GM|Nominal95%block CI|Numerator Go GC cycles(6runs)|Denominator Go GC cycles(6runs)|\n'
      '|---|---:|---|---:|---:|\n']
paired=[]
for r in data['comparisons_ci95']:
    if r['metric']!='process_cpu_ns_per_op':continue
    numerator,denominator=r['numerator'],r['denominator']
    paired.append(dict(r,numerator_go_gc_cycles=gc[numerator],denominator_go_gc_cycles=gc[denominator]))
    text.append(f'|{numerator}/{denominator}|{r["ratio"]:.6f}|[{r["ci95_low"]:.6f},{r["ci95_high"]:.6f}]|{gc[numerator]}|{gc[denominator]}|\n')
if workload in ['batchset50','batchset50x1000','set100']:
    text.append('\nThis frozen base8ab5c5a4d Go write-only commit also omits the causal-read-risky GRV flag that C++ supplies '
                '(NativeAPI.actor.cpp:6577-6578). Go transaction.go:1980-1985 invokes ensureReadVersion; grvFlags at:3398-3411 '
                'only adds this flag on an explicit option. Without the flag the server waits for updateLastCommit '
                '(GrvProxyServer.actor.cpp:647-648). This pre-existing implementation difference is measured, not corrected or quietly stacked into this campaign.\n')
note=''.join(text)
(out/'CPU-SCOPE.md').write_text(note)
for filename in [round.upper()+'-TABLES.md','TABLES.md',round.lower()+'-report.stdout']:
    path=out/filename
    if path.exists():
        with path.open('a') as f:f.write(note)
with (out/'window-cpu-with-gc.csv').open('w') as f:
    writer=csv.DictWriter(f,fieldnames=list(paired[0]));writer.writeheader();writer.writerows(paired)
print(note)
