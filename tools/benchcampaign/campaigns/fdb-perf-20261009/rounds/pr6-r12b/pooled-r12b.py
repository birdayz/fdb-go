#!/usr/bin/env python3
import csv
import importlib.util
import json
import math
from pathlib import Path
import random
import statistics

base=Path('/opt/fdbperf/artifacts')
spec=importlib.util.spec_from_file_location('analyzer','/opt/fdbperf/scripts/astra-perf-analyze.py')
analyzer=importlib.util.module_from_spec(spec)
spec.loader.exec_module(analyzer)
rows=[]
for offset, campaign, name in [(0,'pr6-r12-batchset50','r12'), (6,'pr6-r12b-batchset50','r12b')]:
    data=json.loads((base/campaign/(name+'-detailed.json')).read_text())
    assert len(data['per_run'])==18
    for original in data['per_run']:
        row=dict(original, campaign=campaign, original_block=original['block'],block=original['block']+offset)
        assert row['observations']==2500 and row['errors']==row['retries']==0
        rows.append(row)
by={(r['block'],r['arm']):r for r in rows}
assert len(by)==36
metrics=['mean_ns','p99_ns','process_cpu_ns_per_op','user_cpu_ns_per_op','system_cpu_ns_per_op']
confidence=[]
paired=[]
for numerator,denominator in [('new','old'),('new','cgo'),('old','cgo')]:
    for metric in metrics+(['go_alloc_bytes_per_op','go_mallocs_per_op'] if denominator=='old' else []):
        pairs=[(by[b,numerator][metric],by[b,denominator][metric]) for b in range(12)]
        ci=analyzer.bootstrap_log_ratio(pairs,random.Random(794808),10000)
        confidence.append(dict(numerator=numerator,denominator=denominator,metric=metric,**ci,pairs_below_one=sum(a<b for a,b in pairs)))
        paired.extend(dict(block=b,campaign=by[b,numerator]['campaign'],original_block=by[b,numerator]['original_block'],numerator=numerator,denominator=denominator,metric=metric,ratio=a/b) for b,(a,b) in enumerate(pairs))
text=['# R12 + independent R12b pooled batchset50\n\n12 paired blocks, six from each campaign; 36 runs of2500 measured+200warmup transactions. Same immutable binaries and frozenV6, fresh nonoverlapping prefixes. No profiler overlapped timing. R12b standalone tables are separate. Pooled estimates are descriptive and post-initial-result; not a replacement for the independent repeat.\n\nMedian run statistics; times in microseconds per transaction.\n\n|Arm|Mean|p99|Window CPU|User|System|GoB/tx|Goallocs/tx|GoGC total|\n|---|---:|---:|---:|---:|---:|---:|---:|---:|\n']
for arm in ['old','new','cgo']:
    ar=[r for r in rows if r['arm']==arm]
    text.append('|'+arm+'|'+'|'.join(f'{statistics.median(r[m] for r in ar)/1000:.3f}' for m in metrics)+'|'+'|'.join(f'{statistics.median(r[m] for r in ar):.3f}' for m in ['go_alloc_bytes_per_op','go_mallocs_per_op'])+f'|{sum(r["go_gc_cycles"] for r in ar)}|\n')
text+=['\nWhole-block percentile95%bootstrap CI, 10000resamples, seed794808; common indices across metrics/comparisons. Resamples12blocks without campaign stratification; assumes independent paired blocks, not established on one continuously running VM. No multiplecomparison correction. Ratios geometric within-block, not ratios of medians.\n\n|Ratio|Metric|GM|95%CI|Pairs<1|\n|---|---|---:|---|---:|\n']
for r in confidence:
    text.append(f'|{r["numerator"]}/{r["denominator"]}|{r["metric"]}|{r["ratio"]:.6f}|[{r["ci95_low"]:.6f},{r["ci95_high"]:.6f}]|{r["pairs_below_one"]}/12|\n')
text+=['\nCPU is measured-window getrusage, NOT lifecycle-complete. Native deferred cleanup is not drained by frozenV6; GoGC counts do not establish native cleanup completion. GoMemStats excludes nativeC++ heap. OLD lacks C++CAUSAL_READ_RISKY on missing-readversion commits; NEW fixes precisely that, not otherwise stacked. The current frozen harness exports GRV completion/count and commitstarted/completed counts, not GRV/commit latency aggregates: no latency split was observed or inferred.\n']
out=base/'pr6-r12b-pooled'
out.mkdir(exist_ok=False)
(out/'R12B-POOLED-TABLES.md').write_text(''.join(text))
(out/'pooled.json').write_text(json.dumps(dict(per_run=rows,comparisons_ci95=confidence,per_block=paired,bootstrap_seed=794808,resamples=10000),indent=2)+'\n')
for name,data in [('per-run.csv',rows),('paired.csv',confidence),('per-block.csv',paired)]:
    with (out/name).open('w') as f:
        writer=csv.DictWriter(f,fieldnames=list(data[0]));writer.writeheader();writer.writerows(data)
print(''.join(text))
