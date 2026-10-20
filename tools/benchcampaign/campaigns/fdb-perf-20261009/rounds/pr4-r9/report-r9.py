#!/usr/bin/env python3
import csv
import importlib.util
import random
import json
import math
from pathlib import Path
import statistics
import sys
p=Path(sys.argv[1]);a=json.loads((p/'analysis.json').read_text());plan=json.loads((p/'plan.json').read_text());rows=a['per_run'];w=plan['workloads'][0]
assert len(rows)==18 and len(plan['workloads'])==1 and w['iterations']=={'read1000':5000,'batchset50':2500}[w['name']] and w['warmup']==200 and w['workers']==1
metrics=['mean_ns','p99_ns','process_cpu_ns_per_op','user_cpu_ns_per_op','system_cpu_ns_per_op']
memmetrics=['go_alloc_bytes_per_op','go_mallocs_per_op']
for r in rows:
 s=json.loads((p/r['run_id']/'summary.json').read_text());before,after=s['stats_before'],s['stats_after'];n=r['observations']
 for label in ['user','system']:r[label+'_cpu_ns_per_op']=(after['process_'+label+'_cpu_us']-before['process_'+label+'_cpu_us'])*1000/n
 assert abs(r['user_cpu_ns_per_op']+r['system_cpu_ns_per_op']-r['process_cpu_ns_per_op'])<.0001
 r['go_gc_cycles_per_tx']=r['go_gc_cycles']/n
by={(r['block'],r['arm']):r for r in rows};assert len(by)==18
text=[f'# R9 PR4 inline lone GRV — {w["name"]}\n\nOld=9377c0863aef7ecdee250cdc323e40ebf69d5e46;new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+patch2414b55ab15d20c8597ebc54917b67c24320c8f1b02e8ab6a412a0c1de3be014 (PR1/PR2/PR3 NOT stacked);old9377 code matches590145 in pkg/fdbgo/tools/dependency pins;CGo=immutable native7.3.77. FrozenV6,{w["iterations"]} measured+200warmup/run,workers1,all6three-armorders. Counts wereprespecified,not adapted afterresults.\n\nAbsolute values are medians of6run statistics;ratios are geometric means of6within-block ratios,NOT ratios of medians. Timecolumnsµs/transaction;componentmedians neednotsum. GoMemStats/GC excludeC++heap,so cross-clientallocation comparisonsareinvalid.\n\n|Arm|Mean|p99|CPU|User CPU|System CPU|Go B/tx|Go allocs/tx|Go GC total|Go GC/tx aggregate|\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n']
for arm in ['cgo','old','new']:
 rr=[r for r in rows if r['arm']==arm];assert len(rr)==6
 text.append(f'|{arm}|'+ '|'.join(f'{statistics.median(r[k] for r in rr)/1000:.3f}' for k in metrics)+'|'+ '|'.join(f'{statistics.median(r[k] for r in rr):.3f}' for k in memmetrics)+f'|{sum(r["go_gc_cycles"] for r in rr)}|{sum(r["go_gc_cycles"] for r in rr)/sum(r["observations"] for r in rr):.6f}|\n')
text+=['\n|Ratio|Mean|p99|CPU|User CPU|System CPU|CPU pairs <1|\n|---|---:|---:|---:|---:|---:|---:|\n']
paired=[];block_rows=[];confidence=[]
spec=importlib.util.spec_from_file_location('frozen_analyzer','/opt/fdbperf/scripts/astra-perf-analyze.py')
frozen=importlib.util.module_from_spec(spec);spec.loader.exec_module(frozen)
for numerator,denominator in [('new','old'),('new','cgo'),('old','cgo')]:
 ratios={k:[by[b,numerator][k]/by[b,denominator][k] for b in range(6)] for k in metrics};gm={k:math.exp(statistics.mean(map(math.log,v))) for k,v in ratios.items()}
 text.append(f'|{numerator}/{denominator}|'+ '|'.join(f'{gm[k]:.6f}' for k in metrics)+f'|{sum(x<1 for x in ratios["process_cpu_ns_per_op"])}/6|\n')
 paired.append({'numerator':numerator,'denominator':denominator,'paired_geomean':gm,'per_block_ratios':ratios})
 for metric in metrics:
  ci=frozen.bootstrap_log_ratio([(by[b,numerator][metric],by[b,denominator][metric]) for b in range(6)],random.Random(789707),10000)
  assert abs(ci['ratio']-gm[metric])<1e-12
  confidence.append(dict(numerator=numerator,denominator=denominator,metric=metric,**ci,pairs_below_one=sum(x<1 for x in ratios[metric])))
 for b in range(6):block_rows.append({'block':b,'numerator':numerator,'denominator':denominator,**{k:ratios[k][b] for k in metrics}})
mem={k:[by[b,'new'][k]/by[b,'old'][k] for b in range(6)] for k in memmetrics};memgm={k:math.exp(statistics.mean(map(math.log,v))) for k,v in mem.items()}
text+=['\nGo new/old paired allocation ratios: '+', '.join(f'{k}={memgm[k]:.6f} (below1 in{sum(x<1 for x in mem[k])}/6)' for k in memmetrics)+'.\n']
text+=['\n## Nominal 95% block-bootstrap CIs\n\n10,000 whole-block resamples, seed789707, common resampled indices across metrics/comparisons; six blocks, not individual operations.\n\n|Ratio|Metric|Paired GM|95% CI|Pairs <1|\n|---|---|---:|---|---:|\n']
for r in confidence:text.append(f'|{r["numerator"]}/{r["denominator"]}|{r["metric"]}|{r["ratio"]:.6f}|[{r["ci95_low"]:.6f}, {r["ci95_high"]:.6f}]|{r["pairs_below_one"]}/6|\n')
text+=['\n## Per-block paired ratios\n\n|Block|Ratio|Mean|p99|CPU|User CPU|System CPU|\n|---|---|---:|---:|---:|---:|---:|\n']
for r in block_rows:text.append(f'|{r["block"]}|{r["numerator"]}/{r["denominator"]}|'+ '|'.join(f'{r[k]:.6f}' for k in metrics)+'|\n')
text+=['\n## Per-run GC and allocation\n\n|Run|Go B/tx|Go allocs/tx|Go GC cycles|Go GC/tx|\n|---|---:|---:|---:|---:|\n']
for r in rows:text.append(f'|{r["run_id"]}|{r["go_alloc_bytes_per_op"]:.3f}|{r["go_mallocs_per_op"]:.3f}|{r["go_gc_cycles"]}|{r["go_gc_cycles_per_tx"]:.6f}|\n')
text+=['\nSixblock exploratory singleVM/workload comparison,not capacity/globalnative/noninferiority proof. Nominal bootstrap coverage assumes independent paired blocks,not established on this continuously running server; no multiplicity correction; CI crossing1 is not equivalence. Nativeinternalcounters unobserved. No profilers/buildsoverlappedtiming. Benchmarkraw/persistence/counterchecks and remote standaloneharness tests are NOT parentcorrectness/Bazel/finalreviewapproval.\n']
(p/'R9-TABLES.md').write_text(''.join(text));(p/'r9-detailed.json').write_text(json.dumps({'per_run':rows,'paired':paired,'new_old_allocation_ratios':mem,'new_old_allocation_geomean':memgm,'comparisons_ci95':confidence,'bootstrap_seed':789707,'bootstrap_replicates':10000},indent=2)+'\n')
for name,data in [('r9-per-run.csv',rows),('r9-per-block.csv',block_rows),('r9-confidence.csv',confidence)]:
 with (p/name).open('w') as f:writer=csv.DictWriter(f,fieldnames=list(data[0]));writer.writeheader();writer.writerows(data)
print(''.join(text))
