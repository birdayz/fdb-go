#!/usr/bin/env python3
"""Retain all five CPU snapshots and cleanup evidence beside paired V7 estimates."""
import csv
import json
from pathlib import Path
import statistics

base=Path('/opt/fdbperf/artifacts')
text=['# V7 fresh lifecycle accounting\n\nBaseline8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead and authoritative R11 PR3+PR4+PR5v2 stack each compared with native7.3.77. Four independent campaigns, six paired two-arm blocks each, three of each order. Batch3000/200, read5000/200; GOMAXPROCS8; fresh prefixes. No V6 observations pooled here.\n\nCPU scope: window=(C-B)/N; drained=(D-B)/N after forced-GC finalizer fixpoint/native FIFO barrier; shutdown=(E-A)/N after final drain, DB close and native StopNetwork join. A is after identity hashing but before backend open. Drained includes pending setup/warmup cleanup and forced GC. Shutdown includes client startup,fixture,warmup,persistedvalidation,CSV and forcedGC; excludes prior identity work,finalJSON andexit. AbsoluteE/N includes arm-specific identity work, retained unpaired and not cross-arm client-cost evidence. Orphan native timeout timers may expire after D and are discarded atStopNetwork; D is not terminal shutdown. None is isolated steady-state transaction CPU.\n']
allrows=[]
allcomparisons=[]
for variant in ['baseline','stack']:
    for work in ['batchset50','read1000']:
        campaign='v7-'+variant+'-'+work
        p=base/campaign
        analysis=json.loads((p/'analysis.json').read_text());plan=json.loads((p/'plan.json').read_text())
        assert analysis['valid_zero_error_campaign'] and len(analysis['per_run'])==12 and plan['blocks']==6
        assert not any(r['metric']=='whole_process_cpu_through_shutdown_ns_per_op' for r in analysis['comparisons'])
        text.append('\n## '+campaign+'\n\nAbsolute medians of six run statistics, microseconds per measured transaction. GC columns sum counts across the six processes. These medians are not the paired ratios.\n\n|Arm|Mean|p99|Window CPU|Drained CPU|Shutdown CPU(E-A)|AbsoluteE CPU(unpaired)|Window GC|Drained GC(deltaB)|Shutdown GC(absoluteE)|ForcedGC rounds measurement/shutdown|\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|\n')
        for arm in ['go','cgo']:
            ar=[r for r in analysis['per_run'] if r['arm']==arm];assert len(ar)==6
            mrounds=[];srounds=[]
            for row in ar:
                s=json.loads((p/row['run_id']/'summary.json').read_text());n=s['completed'];life=s['lifecycle']
                assert s['success'] and s['persisted_validation']=='ok' and s['retries']==s['warmup_retries']==0
                assert life['complete'] and life['database_closed'] and life['measurement_barrier_complete']
                assert life['network_stopped']==(arm=='cgo')
                snapshots=[s['stats_after_identity'],s['stats_before'],s['stats_after'],life['stats_after_measurement_drain'],life['stats_after_shutdown']]
                cpus={label:sum(snapshot[k] for k in ['process_user_cpu_us','process_system_cpu_us']) for label,snapshot in zip('ABCDE',snapshots)}
                assert 0<cpus['A']<=cpus['B']<cpus['C']<=cpus['D']<=cpus['E']
                assert abs(row['shutdown_process_cpu_ns_per_op']-(cpus['E']-cpus['A'])*1000/n)<1e-6
                assert abs(row['whole_process_cpu_through_shutdown_ns_per_op']-cpus['E']*1000/n)<1e-6
                md,sd=life['measurement_drain'],life['shutdown_drain']
                for drain in [md,sd]:
                    assert drain['complete'] and drain['after']['finalizers_queued']==drain['after']['finalizers_executed']
                    assert drain['before']['cleanups_queued_estimate']==drain['after']['cleanups_queued_estimate']==drain['after']['cleanups_executed_estimate']
                mrounds.append(md['rounds']);srounds.append(sd['rounds'])
                allrows.append(dict(campaign=campaign,**row,**{'cpu_'+k+'_us':v for k,v in cpus.items()},measurement_forced_gc_rounds=md['rounds'],shutdown_forced_gc_rounds=sd['rounds'],measurement_finalizers_queued=md['after']['finalizers_queued'],measurement_finalizers_executed=md['after']['finalizers_executed'],shutdown_finalizers_queued=sd['after']['finalizers_queued'],shutdown_finalizers_executed=sd['after']['finalizers_executed']))
            times=['mean_ns','p99_ns','process_cpu_ns_per_op','drained_cpu_ns_per_op','shutdown_process_cpu_ns_per_op','whole_process_cpu_through_shutdown_ns_per_op']
            text.append('|'+arm+'|'+'|'.join(f'{statistics.median(r[m] for r in ar)/1000:.3f}' for m in times)+'|'+'|'.join(str(sum(r[m] for r in ar)) for m in ['go_gc_cycles','drained_go_gc_cycles','shutdown_go_gc_cycles'])+'|'+str(mrounds)+' / '+str(srounds)+'|\n')
        text.append('\n|Go/CGo metric|Paired GM|Nominal95%block CI|Pairs<1|\n|---|---:|---|---:|\n')
        for r in analysis['comparisons']:
            allcomparisons.append(dict(campaign=campaign,**r))
            if r['metric'] not in ['mean_ns','p99_ns','process_cpu_ns_per_op','drained_cpu_ns_per_op','shutdown_process_cpu_ns_per_op']:continue
            text.append(f'|{r["metric"]}|{r["ratio"]:.6f}|[{r["ci95_low"]:.6f},{r["ci95_high"]:.6f}]|{r["pairs_below_one"]}/6|\n')
text.append('\n10,000 paired-block resamples, seed642001 per campaign, percentile95% intervals; not operation-resampling. Six same-VM exploratory blocks do not establish independence or nominal coverage; no multiplicity correction, equivalence/noninferiority/globalcapacity claim. GoGC includes forced accounting work, whose round count can differ by arm. GoMemStats excludes nativeC++ heap. Baseline and stack both retain the pre-existing missing CAUSAL_READ_RISKY flag on write-only commit GRVs; PR6 is NOT stacked.\n')
assert len(allrows)==48
out=base/'lifecycle-v7-summary';out.mkdir(exist_ok=False)
(out/'V7-TABLES.md').write_text(''.join(text))
(out/'lifecycle-evidence.json').write_text(json.dumps(dict(per_run=allrows,comparisons=allcomparisons),indent=2)+'\n')
for name,rows in [('per-run.csv',allrows),('paired.csv',allcomparisons)]:
    with (out/name).open('w') as f:
        writer=csv.DictWriter(f,fieldnames=list(rows[0]));writer.writeheader();writer.writerows(rows)
print(''.join(text))
