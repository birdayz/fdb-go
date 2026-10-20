#!/usr/bin/env python3
import csv
import json
import math
from pathlib import Path
import statistics
import sys
p=Path(sys.argv[1]);a=json.loads((p/'analysis.json').read_text());plan=json.loads((p/'plan.json').read_text());rows=a['per_run']
assert len(rows)==18 and plan['workloads']==[{'name':'batchset50','iterations':2500,'warmup':200,'workers':1}]
metrics=['mean_ns','p99_ns','process_cpu_ns_per_op','user_cpu_ns_per_op','system_cpu_ns_per_op']
for r in rows:
 s=json.loads((p/r['run_id']/'summary.json').read_text());before,after=s['stats_before'],s['stats_after'];n=r['observations']
 for label in ['user','system']:r[label+'_cpu_ns_per_op']=(after['process_'+label+'_cpu_us']-before['process_'+label+'_cpu_us'])*1000/n
 assert abs(r['user_cpu_ns_per_op']+r['system_cpu_ns_per_op']-r['process_cpu_ns_per_op'])<.0001
 r['go_gc_cycles_per_tx']=r['go_gc_cycles']/n
by={(r['block'],r['arm']):r for r in rows};assert len(by)==18
text=['# R5 PR1 keyrange memcmp — unprofiled\n\nOld=9377c0863aef7ecdee250cdc323e40ebf69d5e46; new=old+patch33f1ed2062a8f412f0ee291af16379f41e32aa7edee5b4274d1db1f7fc7b96ef; CGo=immutable native7.3.77. FrozenV6,2500 measured+200warmup/run,workers1,all6three-armorders.2500was chosen before execution fromR3wallbudget,not after rerunning results.\n\nAbsolute values are medians of6run statistics (µs/transaction); ratios are geometric means of6within-block ratios,NOT ratios of medians. Component medians need not sum. Go GC counts cover Go heap only; native heap is unobserved.\n\n|Arm|Mean|p99|CPU|User CPU|System CPU|Go GC total|Go GC/tx aggregate|\n|---|---:|---:|---:|---:|---:|---:|---:|\n']
for arm in ['cgo','old','new']:
 rr=[r for r in rows if r['arm']==arm];assert len(rr)==6
 text.append(f'|{arm}|'+ '|'.join(f'{statistics.median(r[k] for r in rr)/1000:.3f}' for k in metrics)+f'|{sum(r["go_gc_cycles"] for r in rr)}|{sum(r["go_gc_cycles"] for r in rr)/sum(r["observations"] for r in rr):.6f}|\n')
text+=['\n|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|\n|---|---:|---:|---:|---:|---:|---:|\n']
paired=[];block_rows=[]
for numerator,denominator in [('new','old'),('new','cgo'),('old','cgo')]:
 ratios={k:[by[b,numerator][k]/by[b,denominator][k] for b in range(6)] for k in metrics};gm={k:math.exp(statistics.mean(map(math.log,v))) for k,v in ratios.items()}
 text.append(f'|{numerator}/{denominator}|'+ '|'.join(f'{gm[k]:.6f}' for k in metrics)+f'|{sum(x<1 for x in ratios["process_cpu_ns_per_op"])}/6|\n')
 paired.append({'numerator':numerator,'denominator':denominator,'paired_geomean':gm,'per_block_ratios':ratios})
 for b in range(6):block_rows.append({'block':b,'numerator':numerator,'denominator':denominator,**{k:ratios[k][b] for k in metrics}})
text+=['\n## Per-block paired ratios\n\n|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|\n|---|---|---:|---:|---:|---:|---:|\n']
for r in block_rows:text.append(f'|{r["block"]}|{r["numerator"]}/{r["denominator"]}|'+ '|'.join(f'{r[k]:.6f}' for k in metrics)+'|\n')
text+=['\n## Per-run GC and allocation\n\n|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|\n|---|---:|---:|---:|---:|\n']
for r in rows:text.append(f'|{r["run_id"]}|{r["go_alloc_bytes_per_op"]:.3f}|{r["go_mallocs_per_op"]:.3f}|{r["go_gc_cycles"]}|{r["go_gc_cycles_per_tx"]:.6f}|\n')
text+=['\nSixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Native internal counters unobserved;GoMemStats excludeC++heap,so no Go/CGoallocation ratios. No profilers/builds overlappedtiming. Benchmark byte/persistence/counterchecks and remote standalone harness tests are NOT parent correctness/Bazel/finalreview approval.\n']
(p/'R5-TABLES.md').write_text(''.join(text));(p/'r5-detailed.json').write_text(json.dumps({'per_run':rows,'paired':paired},indent=2)+'\n')
for name,data in [('r5-per-run.csv',rows),('r5-per-block.csv',block_rows)]:
 with (p/name).open('w') as f:w=csv.DictWriter(f,fieldnames=list(data[0]));w.writeheader();w.writerows(data)
print(''.join(text))
