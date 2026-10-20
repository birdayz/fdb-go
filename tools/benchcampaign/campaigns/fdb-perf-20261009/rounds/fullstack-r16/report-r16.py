#!/usr/bin/env python3
import csv
import importlib.util
import json
import math
from pathlib import Path
import random
import statistics
import sys

p=Path(sys.argv[1]);a=json.loads((p/'analysis.json').read_text());plan=json.loads((p/'plan.json').read_text())
assert a['valid_zero_error_campaign'] and len(a['per_run'])==18 and plan['blocks']==6
assert len(plan['workloads'])==1;w=plan['workloads'][0]
assert w==dict(name=w['name'],iterations=5000 if w['name']=='read1000' else 2500,warmup=200,workers=1)
rows=a['per_run']
for row in rows:
    s=json.loads((p/row['run_id']/'summary.json').read_text());life=s['lifecycle'];native=row['arm']=='cgo';n=s['completed']
    assert s['success'] and s['retries']==s['warmup_retries']==0 and s['persisted_validation']=='ok'
    assert life['complete'] and life['database_closed'] and life['measurement_barrier_complete'] and life['network_stopped']==native
    snapshots=[s['stats_after_identity'],s['stats_before'],s['stats_after'],life['stats_after_measurement_drain'],life['stats_after_shutdown']]
    cpus=[sum(v[k] for k in ['process_user_cpu_us','process_system_cpu_us']) for v in snapshots]
    assert 0<cpus[0]<=cpus[1]<cpus[2]<=cpus[3]<=cpus[4]
    assert abs(row['shutdown_process_cpu_ns_per_op']-(cpus[4]-cpus[0])*1000/n)<1e-6
    assert abs(row['whole_process_cpu_through_shutdown_ns_per_op']-cpus[4]*1000/n)<1e-6
    row.update({f'cpu_{label}_us':v for label,v in zip('ABCDE',cpus)})
    row['shutdown_user_cpu_ns_per_op']=(snapshots[4]['process_user_cpu_us']-snapshots[0]['process_user_cpu_us'])*1000/n
    row['shutdown_system_cpu_ns_per_op']=(snapshots[4]['process_system_cpu_us']-snapshots[0]['process_system_cpu_us'])*1000/n
    for short,key in [('measurement','measurement_drain'),('shutdown','shutdown_drain')]:
        drain=life[key];after=drain['after']
        assert drain['complete'] and after['finalizers_queued']==after['finalizers_executed']
        assert drain['before']['cleanups_queued_estimate']==after['cleanups_queued_estimate']==after['cleanups_executed_estimate']
        row[short+'_forced_gc_rounds']=drain['rounds'];row[short+'_finalizers_queued']=after['finalizers_queued'];row[short+'_finalizers_executed']=after['finalizers_executed']
    if not native:
        counters=s['client_counters_delta']['values']
        assert counters['transaction_retries']==counters['grv_cache_hits']==counters['connection_failures']==0
        assert counters['read_versions_completed']==counters['grv_successful_batch_replies']==n
        assert counters['commit_started']==counters['commit_completed']==(0 if w['name']=='read1000' else n)
by={(r['block'],r['arm']):r for r in rows};assert len(by)==18
metrics=['mean_ns','p99_ns','process_cpu_ns_per_op','user_cpu_ns_per_op','system_cpu_ns_per_op','drained_cpu_ns_per_op','shutdown_process_cpu_ns_per_op','shutdown_user_cpu_ns_per_op','shutdown_system_cpu_ns_per_op']
absolute_metrics=metrics+['whole_process_cpu_through_shutdown_ns_per_op']
memmetrics=['go_alloc_bytes_per_op','go_mallocs_per_op']
text=[f'# R16 cumulative fullstack — {w["name"]}\n\n{plan["purpose"]}\n\n{w["iterations"]}measured+200warmup,workers1,18runs/sixthree-armorders. Absolute times are medians of6run statistics in microseconds per transaction. AbsoluteE CPU is unpaired only; no cross-arm client-cost interpretation. GoMemStats excludes nativeC++ heap.\n\n|Arm|Mean|p99|Window CPU|User|System|Drained CPU|Shutdown(E-A) CPU|Shutdown(E-A) User|Shutdown(E-A) System|AbsoluteE CPU(unpaired)|GoB/tx|Goallocs/tx|GoGC window/drained/shutdown|ForcedGC rounds measure/shutdown|\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---|\n']
for arm in ['old','new','cgo']:
    ar=[r for r in rows if r['arm']==arm];assert len(ar)==6
    text.append('|'+arm+'|'+'|'.join(f'{statistics.median(r[k] for r in ar)/1000:.3f}' for k in absolute_metrics)+'|'+'|'.join(f'{statistics.median(r[k] for r in ar):.3f}' for k in memmetrics)+'|'+'/'.join(str(sum(r[k] for r in ar)) for k in ['go_gc_cycles','drained_go_gc_cycles','shutdown_go_gc_cycles'])+'|'+str([r['measurement_forced_gc_rounds'] for r in ar])+' / '+str([r['shutdown_forced_gc_rounds'] for r in ar])+'|\n')
scripts='/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts'
sys.path.insert(0,scripts)
spec=importlib.util.spec_from_file_location('analyzer',scripts+'/analyze.py');analyzer=importlib.util.module_from_spec(spec);spec.loader.exec_module(analyzer)
confidence=[];blockrows=[]
for numerator,denominator in [('new','old'),('new','cgo'),('old','cgo')]:
    for metric in metrics+(memmetrics if denominator=='old' else []):
        pairs=[(by[b,numerator][metric],by[b,denominator][metric]) for b in range(6)]
        assert all(v>0 for pair in pairs for v in pair)
        ci=analyzer.bootstrap_log_ratio(pairs,random.Random(796707),10000)
        confidence.append(dict(numerator=numerator,denominator=denominator,metric=metric,**ci,pairs_below_one=sum(x<y for x,y in pairs)))
        blockrows.extend(dict(block=block,numerator=numerator,denominator=denominator,metric=metric,ratio=x/y) for block,(x,y) in enumerate(pairs))
text+=['\nPaired geometric means of6within-block ratios, NOT ratios of displayedmedians; percentile95% whole-block bootstrap CI,10000resamples,seed796707,common resampledindices acrossmetrics/comparisons.\n\n|Ratio|Metric|GM|95%CI|Pairs<1|\n|---|---|---:|---|---:|\n']
for r in confidence:
    text.append(f'|{r["numerator"]}/{r["denominator"]}|{r["metric"]}|{r["ratio"]:.6f}|[{r["ci95_low"]:.6f},{r["ci95_high"]:.6f}]|{r["pairs_below_one"]}/6|\n')
text+=['\nV7 scopes: window=(C-B)/N; drained=(D-B)/N includes forcedGC and pending setup/warmup cleanup after nativeFIFO barrier; shutdown=(E-A)/N includes client startup,fixture,warmup,validation,CSV and forcedGC through DBclose/nativeStopNetworkjoin. A excludes earlier arm-specific executable/libraryhashing. AbsoluteE/N is raw/absolute-only, never paired. FinalJSON/process exit excluded. Drains2or moreforcedGCs may differbyarm. Notisolatedsteady-stateCPU. SixsameVM exploratoryblocks do not establish independence/nominalCIcoverage; no multiplicitycorrection or equivalence/noninferiority/globalcapacityclaim. Combinedpatchparentcorrectness/reviews/CI are not proved by benchmarksuccess.\n']
(p/'R16-TABLES.md').write_text(''.join(text))
(p/'r16-detailed.json').write_text(json.dumps(dict(per_run=rows,comparisons_ci95=confidence,per_block=blockrows,bootstrap_seed=796707,resamples=10000),indent=2)+'\n')
for name,data in [('r16-per-run.csv',rows),('r16-confidence.csv',confidence),('r16-per-block.csv',blockrows)]:
    with (p/name).open('w') as f:
        writer=csv.DictWriter(f,fieldnames=list(data[0]));writer.writeheader();writer.writerows(data)
print(''.join(text))
