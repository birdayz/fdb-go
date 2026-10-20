#!/usr/bin/env python3
"""Untimed pre-P0 capacity/correctness smokes; never input to campaign analysis."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

R=Path('/srv/fdb-bench');T=R/'tooling/tools/benchcampaign/multinode'
sys.path.insert(0,str(T));import cluster_gate as cg


def dump(path,value):
    with Path(path).open('x') as f:json.dump(value,f,indent=2);f.write('\n')


def main():
    os.umask(0o077)
    parser=argparse.ArgumentParser();parser.add_argument('--output',type=Path,required=True);parser.add_argument('--only-master',action='store_true');parser.add_argument('--expect-inputs-sha256',required=True);parser.add_argument('--phase',action='append',choices=('M1w1','M1w64','M1w8','M1w32'));args=parser.parse_args()
    inputs_path=R/'provenance/capacity-inputs.json'
    assert hashlib.sha256(inputs_path.read_bytes()).hexdigest()==args.expect_inputs_sha256
    pins=json.loads(inputs_path.read_text())
    def verify_pins():
        for path,digest in pins.items():assert hashlib.sha256(Path(path).read_bytes()).hexdigest()==digest,'Pinned capacity input changed: '+path
    verify_pins()
    out=args.output;assert out.is_relative_to(R/'diagnostic');out.mkdir(parents=True,exist_ok=False)
    proto=json.loads((R/'protocol.json').read_text());layout=json.loads((R/'cluster-ssd/layout.json').read_text());cf=R/'cluster-ssd/fdb.cluster';cg.cluster_file_guard(cf,layout,'10.89.20.0/24')
    sys.path.insert(0,str(R/'execution/placement'));import seal_live
    source=json.loads((R/'provenance/source-receipt.json').read_text());seal_live.check_revisions(proto,source)
    compat=seal_live.harness_compatibility(R,proto,source);assert compat
    seal_live.artifact_pins(R,source,compat)
    harness=compat['archive_sha256']
    dump(out/'inputs.json',{'expected_inputs_sha256':args.expect_inputs_sha256,'pins':pins,'harness_compatibility':compat,'only_master_smoke':args.only_master,'selected_phases':args.phase})
    revs=proto['production_revisions'];arms=[('before','client-previous-go',revs['before_allocation_work_after_read_parity']),('final','client-master-go',revs['final']),('cgo','client-master-cgo',revs['final'])]
    env=dict(os.environ,GOMAXPROCS='8',GOGC='100',GOMEMLIMIT='off',GODEBUG='',TZ='UTC',LC_ALL='C')
    if args.only_master:arms=[a for a in arms if a[0]=='final']
    phase_reports=[]
    def health(folder):
        for attempt in range(13):
            p=subprocess.run(['/usr/bin/fdbcli','-C',str(cf),'--exec','status json'],capture_output=True,text=True,check=True,timeout=30);status=json.loads(p.stdout)
            dump(folder/f'health-{attempt:02d}.json',status)
            errors,movement,_=cg.status_verdict(status,layout,'ssd-2','enabled')
            if not errors and not movement:return
            if attempt==12:raise RuntimeError('capacity health gate: '+repr(errors))
            time.sleep(5)
    for phase_id in (('M1w1',) if args.only_master else (args.phase or ('M1w1','M1w64','M1w8','M1w32'))):
        phase=next(p for p in proto['phases'] if p['id']==phase_id);case_reports=[]
        for workload in (['grv'] if args.only_master else phase['workloads']):
            case=out/phase_id/workload;case.mkdir(parents=True)
            clear=subprocess.run(['/usr/bin/fdbcli','-C',str(cf),'--exec','writemode on; clearrange "" "\\xff"'],capture_output=True,text=True,check=True,timeout=30)
            (case/'clear.txt').write_text(clear.stdout+clear.stderr);assert 'Committed' in clear.stdout
            elapsed=[];n=32 if args.only_master else phase['iterations'];warm=4 if args.only_master else phase['warmup']
            for name,binary,revision in arms:
                folder=case/name;folder.mkdir();verify_pins();health(folder);time.sleep(3)
                result=folder/'run';argv=['taskset','-c','8-15',str(R/'bin'/binary),'--cluster-file',str(cf),'--workload',workload,'--iterations',str(n),'--warmup',str(warm),'--workers',str(phase['workers']),'--output',str(result),'--run-id',phase_id+'-'+workload+'-'+name,'--block','0','--source-revision',revision,'--harness-id',harness,'--allow-writes','--prefix',f'clientbench/capacity/{phase_id}/{workload}/{name.ljust(6,"_")}/']
                start=time.monotonic()
                with (folder/'stdout').open('xb') as stdout,(folder/'stderr').open('xb') as stderr:
                    p=subprocess.run(argv,env=env,stdout=stdout,stderr=stderr,timeout=180)
                seconds=time.monotonic()-start;verify_pins();dump(folder/'command.json',{'argv':argv,'returncode':p.returncode,'wall_seconds':seconds,'scope':'UNTIMED capacity diagnostic, no comparative estimates'})
                if p.returncode:raise RuntimeError('capacity correctness/operation failure '+str(folder))
                assert (result/'manifest.json').is_file() and (result/'summary.json').is_file()
                summary=json.loads((result/'summary.json').read_text())
                assert summary['completed']==n and summary['errors']==0 and not summary['failure'], summary
                # Full-count capacity runs use the exact planned process wall.
                # The separate32-op compatibility check is not a capacity result.
                estimated=seconds*(phase['iterations']+phase['warmup'])/(n+warm)
                elapsed.append({'arm':name,'smoke_seconds':seconds,'full_process_estimate_seconds':estimated})
            worst=max(e['full_process_estimate_seconds'] for e in elapsed)
            predicted=1.3*phase['blocks']*len(phase['arms'])*(worst+3+2)
            record={'workload':workload,'smoke_iterations':n,'smoke_warmup':warm,'arms':elapsed,'conservative_campaign_seconds':predicted,'cap_seconds':phase['per_workload_campaign_wall_cap_seconds'],'feasible':predicted<=phase['per_workload_campaign_wall_cap_seconds']}
            dump(case/'capacity.json',record);case_reports.append(record)
            print(phase_id,workload,'capacity_seconds',round(predicted,2),'feasible',record['feasible'],flush=True)
        total=sum(c['conservative_campaign_seconds'] for c in case_reports)
        phase_reports.append({'phase':phase_id,'cases':case_reports,'conservative_phase_seconds':total,'cap_seconds':phase['wall_cap_seconds'],'feasible':all(c['feasible'] for c in case_reports) and total<=phase['wall_cap_seconds']})
    dump(out/'complete.json',{'scope':'PRE-TIMING capacity only; not campaign samples','only_master_smoke':args.only_master,'phases':phase_reports,'all_feasible':all(p['feasible'] for p in phase_reports)})
    if not all(p['feasible'] for p in phase_reports):raise RuntimeError('capacity infeasible; needs protocol decision BEFORE timing')
    print('CLEANUP-COMPAT SMOKE PASS' if args.only_master else 'CAPACITY PASS: all workloads and phases fit conservative caps',flush=True)


if __name__=='__main__':main()
