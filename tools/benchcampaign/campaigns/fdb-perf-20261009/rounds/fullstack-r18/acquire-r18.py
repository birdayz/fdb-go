#!/usr/bin/env python3
"""Execute frozen R18 timings, then two separate traces; never analyze inline."""
import csv
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import time

ROOT=Path('/opt/fdbperf/artifacts/fullstack-r18')
SCRIPTS=Path('/opt/fdbperf/sources/r17-frozen-harness/tools/clientbench/scripts')
spec=importlib.util.spec_from_file_location('r18_runner',SCRIPTS/'run.py')
runner=importlib.util.module_from_spec(spec);spec.loader.exec_module(runner)
process=None

def read(path):return json.loads(path.read_text())
def verify_inputs(pins):
    for path,expected in pins.items():assert runner.sha(path)==expected,path

def check_observations(folder,entry):
    runner.validate_raw_output(folder,entry['workload'])
    summary=read(folder/'summary.json')
    for key in ['errors','warmup_errors','retries','warmup_retries']:
        assert type(summary[key]) is int and summary[key]==0,(folder,key)
    assert summary['failure']=='' and summary['warmup_requested']==200
    for filename,count in [('samples.csv',entry['workload']['iterations']),('warmup.csv',200)]:
        with (folder/filename).open(newline='') as f:rows=list(csv.DictReader(f))
        assert len(rows)==count and all(row['attempts']=='1' for row in rows),(folder,filename)
    if entry['arm']!='cgo':
        c=summary['client_counters_delta']['values'];n=entry['workload']['iterations']
        assert all(type(c[k]) is int and c[k]==0 for k in ['transaction_retries','connection_failures','grv_cache_hits'])
        assert c['read_versions_completed']==c['grv_successful_batch_replies']==n
        assert c['commit_started']==c['commit_completed']==0

def reserve_process():
    if time.monotonic()+180>deadline:raise TimeoutError('R18 cap: insufficient remaining time for another bounded process')

manifest=read(ROOT/'execution-manifest.json');seal=runner.sha(ROOT/'execution-manifest.json')
assert manifest['schema']=='r18-diagnostics-v1'
assert len(manifest['entries'])==manifest['scheduled_timing_runs']==18
assert len(manifest['traces'])==manifest['scheduled_trace_runs']==2
assert sorted(os.sched_getaffinity(0))==[0,1]
assert not any(k.startswith('FDB_NETWORK_OPTION_') for k in os.environ)
assert not any((ROOT/name).exists() for name in ['STARTED.json','ABORTED.json','complete.json','timing-complete.json','strace-sequence'])
for item in manifest['entries']+manifest['traces']:
    assert runner.sha(ROOT/item['plan'])==item['plan_sha256']
    assert runner.sha(ROOT/item['schedule'])==item['schedule_sha256']
    assert read(ROOT/item['plan'])['expected_sha256']==manifest['input_sha256']
for item in manifest['entries']:assert not (ROOT/item['name']).exists()
verify_inputs(manifest['input_sha256'])
binaries={a['binary'] for a in read(ROOT/manifest['global_plan'])['arms']}
for task in Path('/proc').glob('[0-9]*'):
    try:executable=os.readlink(task/'exe')
    except (FileNotFoundError,ProcessLookupError,PermissionError):continue
    assert executable not in binaries,('competing benchmark',task,executable)
runner.dump(ROOT/'STARTED.json',dict(started_utc=runner.utc(),execution_manifest_sha256=seal))
deadline=time.monotonic()+manifest['timing_trace_cap_seconds']
completed_timing=completed_traces=0
for sig in [signal.SIGTERM,signal.SIGINT]:signal.signal(sig,runner.interrupted)
try:
    for item in manifest['entries']:
        reserve_process()
        assert runner.sha(ROOT/'execution-manifest.json')==seal
        plan=read(ROOT/item['plan']);schedule=read(ROOT/item['schedule']);assert len(schedule)==1
        folder=ROOT/item['name'];folder.mkdir()
        runner.dump(folder/'plan.json',plan);runner.dump(folder/'schedule.json',schedule)
        assert runner.sha(folder/'plan.json')==item['plan_sha256']
        assert runner.sha(folder/'schedule.json')==item['schedule_sha256']
        print('TIMING_BEGIN '+item['name']+' '+runner.utc(),flush=True)
        rc=runner.execute(plan,schedule,str(ROOT/item['plan']),folder,min(time.monotonic()+180,deadline))
        assert rc==0 and read(folder/'complete.json')['completed']==1
        check_observations(folder/schedule[0]['run_id'],schedule[0])
        completed_timing+=1
        runner.dump(ROOT/'progress.json',dict(completed_timing_runs=completed_timing,completed_trace_runs=0))
        print(f'TIMING_COMPLETE {completed_timing}/18 '+runner.utc(),flush=True)
    runner.dump(ROOT/'timing-complete.json',dict(completed_timing_runs=18,scheduled_timing_runs=18,execution_manifest_sha256=seal,finished_utc=runner.utc()))
    (ROOT/'strace-sequence').mkdir()
    for item in manifest['traces']:
        reserve_process();verify_inputs(manifest['input_sha256']);runner.check_builds()
        plan=read(ROOT/item['plan']);entry=read(ROOT/item['schedule'])[0]
        folder=ROOT/'strace-sequence'/item['name'];folder.mkdir()
        runner.dump(folder/'plan.json',plan)
        runner.dump(folder/'invocation.json',dict(command=item['command'],entry=entry))
        time.sleep(2);gate=runner.quiescence(plan);runner.dump(folder/'quiescence.json',gate)
        if not gate['passed']:raise RuntimeError('R18 trace quiescence failure: '+item['name'])
        runner.snapshot(folder/'before');started=runner.utc();start=time.monotonic_ns()
        env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','HOME':str(Path.home()),**plan['environment']}
        print('TRACE_BEGIN '+item['name']+' '+started,flush=True)
        with (folder/'stdout.txt').open('w') as stdout,(folder/'stderr.txt').open('w') as stderr,(folder/'telemetry.jsonl').open('w') as telemetry,(folder/'thread-names.jsonl').open('w') as threads:
            process=subprocess.Popen(item['command'],env=env,stdout=stdout,stderr=stderr,start_new_session=True)
            process_deadline=time.monotonic()+180
            while True:
                telemetry.write(json.dumps(runner.telemetry(process.pid),sort_keys=True)+'\n');telemetry.flush()
                if (folder/'manifest.json').exists():
                    try:
                        pid=read(folder/'manifest.json')['pid'];names={}
                        for task in Path('/proc/'+str(pid)+'/task').iterdir():
                            try:names[task.name]=(task/'comm').read_text().strip()
                            except (FileNotFoundError,ProcessLookupError):pass
                        threads.write(json.dumps(dict(utc=runner.utc(),pid=pid,names=names),sort_keys=True)+'\n');threads.flush()
                    except (FileNotFoundError,ProcessLookupError,json.JSONDecodeError):pass
                remaining=min(process_deadline,deadline)-time.monotonic()
                if remaining<=0:raise TimeoutError('R18 trace timeout: '+item['name'])
                try:process.wait(timeout=min(1,remaining));break
                except subprocess.TimeoutExpired:pass
        status=process.returncode;runner.stop_process_group(process);process=None
        runner.snapshot(folder/'after')
        runner.dump(folder/'result.json',dict(run_id=entry['run_id'],returncode=status,started_utc=started,ended_utc=runner.utc(),process_wall_ns=time.monotonic_ns()-start))
        assert status==0,('trace process failed',item['name'],status)
        check_observations(folder,entry);runner.validate_identity(folder,entry,plan,manifest['input_sha256'])
        assert list(folder.glob('trace.[0-9]*'))
        completed_traces+=1
        runner.dump(ROOT/'progress.json',dict(completed_timing_runs=completed_timing,completed_trace_runs=completed_traces))
        print(f'TRACE_COMPLETE {completed_traces}/2 '+runner.utc(),flush=True)
    verify_inputs(manifest['input_sha256'])
    assert runner.sha(ROOT/'execution-manifest.json')==seal and time.monotonic()<=deadline
    assert completed_timing==18 and completed_traces==2
    runner.dump(ROOT/'strace-sequence/complete.json',dict(completed=2,scope='diagnostic phase/wholeprocess counts only; never instrumented timing claims'))
    runner.dump(ROOT/'complete.json',dict(schema='r18-diagnostics-v1',completed_timing_runs=18,scheduled_timing_runs=18,
        completed_trace_runs=2,scheduled_trace_runs=2,execution_manifest_sha256=seal,finished_utc=runner.utc()))
    print('R18 COMPLETE: 18 timings + 2 separate traces. Analysis may begin.',flush=True)
except BaseException as error:
    runner.stop_process_group(process);process=None
    runner.dump(ROOT/'ABORTED.json',dict(error=repr(error),completed_timing_runs=completed_timing,completed_trace_runs=completed_traces,
        execution_manifest_sha256=seal,finished_utc=runner.utc()))
    raise
finally:runner.stop_process_group(process)
