#!/usr/bin/env python3
"""Serial V7 syscall diagnostics after all R16 timing; no instrumented timing claims."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import time

scripts=Path('/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts')
spec=importlib.util.spec_from_file_location('runner',scripts/'run.py');runner=importlib.util.module_from_spec(spec);spec.loader.exec_module(runner)
source=Path('/opt/fdbperf/artifacts/fullstack-r16-read1000')
plan=json.loads((source/'plan.json').read_text());schedule=json.loads((source/'schedule.json').read_text());pins=json.loads((source/'binaries.json').read_text())
for work in ['read1000','batchset50']:
    done=json.loads((Path('/opt/fdbperf/artifacts/fullstack-r16-'+work)/'complete.json').read_text())
    assert done['completed']==done['scheduled']==18
assert sorted(os.sched_getaffinity(0))==[0,1]
for path,digest in pins.items():assert runner.sha(path)==digest,path
out=Path('/opt/fdbperf/artifacts/fullstack-r16-diagnostics');out.mkdir(exist_ok=False)
capability=json.loads(Path('/opt/fdbperf/artifacts/fullstack-r16-build/perf-capability.json').read_text())
runner.dump(out/'perf-capability.json',capability)
trace_calls=['read','write','readv','writev','pread64','pwrite64','sendmsg','recvfrom','sendto','recvmsg','recvmmsg','sendmmsg','epoll_pwait','epoll_wait','epoll_ctl','futex','nanosleep','clock_nanosleep','sched_yield','getrusage']
runner.dump(out/'scope.json',dict(process_population='one NEW then one CGo process per instrument, all serial after timing',
    whole_counts='strace summary and perf stat cover startup including identity,fixture,warmup,measurement,drains,validation,CSV,shutdown and exit; divide by5000 as wholeprocess/N, never phase counts',
    sequence='Separate200measured+200warmup processes, timestamped per-TID files. Five getrusage results identify A..E by exact user/system/maxRSS tuple. Measured-phase entry counts lie after B syscall return and before C syscall entry. Unknown sample epoch gap after B prevents exact per-operation boundary alignment.',
    caveat='Instrumentation perturbs scheduling/syscall mix. No instrumented timing conclusions; native internal RPC retries are unobserved, callback retries checked separately.',
    trace_calls=trace_calls,per_process_timeout_seconds=180))
runner.dump(out/'binaries.json',pins)
runner.dump(out/'tools.json',{path:runner.sha(path) for path in [__file__,'/usr/bin/strace','/usr/bin/perf','/usr/bin/time','/usr/bin/taskset']})
process=None;completed=0;skipped=0
for sig in (signal.SIGTERM,signal.SIGINT):signal.signal(sig,runner.interrupted)
try:
    for instrument,count,namespace in [('strace-summary',5000,'b7163'+'0'*24),('perf-stat',5000,'b7164'+'0'*24),('strace-sequence',200,'b7165'+'0'*24)]:
        root=out/instrument;root.mkdir()
        for arm in ['new','cgo']:
            folder=root/arm;folder.mkdir()
            if instrument=='perf-stat' and not capability['selected_events']:
                runner.dump(folder/'SKIPPED.json',dict(reason='Neither requested tracepoint event group nor standard events were permitted; see perf-capability.json. No zero measurements substituted.'))
                skipped+=1;continue
            runner.check_builds()
            for path,digest in pins.items():assert runner.sha(path)==digest,path
            time.sleep(2);gate=runner.quiescence(plan);runner.dump(folder/'quiescence.json',gate)
            if not gate['passed']:raise RuntimeError('diagnostic quiescence failed: '+instrument+' '+arm)
            entry=copy.deepcopy(next(r for r in schedule if r['arm']==arm and r['block']==0))
            entry['run_id']=instrument+'-'+arm;entry['workload']['iterations']=count
            prefix='clientbench/'+namespace+'/read1000________/b000/'+arm+'/'
            assert len(prefix.encode())==68
            command=entry['command']
            for flag,value in [('--iterations',str(count)),('--warmup','200'),('--output',str(folder)),('--run-id',entry['run_id']),('--prefix',prefix)]:command[command.index(flag)+1]=value
            if instrument=='strace-summary':wrapper=['/usr/bin/strace','-f','-c','-S','calls','-o',str(folder/'strace-summary.txt'),'--']
            elif instrument=='perf-stat':wrapper=['/usr/bin/perf','stat','-x,','--no-big-num','-e',','.join(capability['selected_events']),'-o',str(folder/'perf-stat.csv'),'--']
            else:wrapper=['/usr/bin/strace','-ff','-ttt','-T','-v','-s','128','-e','trace='+','.join(trace_calls),'-o',str(folder/'trace'),'--']
            invocation=['/usr/bin/time','-v','-o',str(folder/'process-time.txt'),'--']+wrapper+command
            runner.dump(folder/'invocation.json',dict(command=invocation,entry=entry))
            env={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','HOME':str(Path.home()),**plan['environment']}
            runner.snapshot(folder/'before');started=runner.utc();start=time.monotonic_ns()
            with (folder/'stdout.txt').open('w') as stdout,(folder/'stderr.txt').open('w') as stderr,(folder/'telemetry.jsonl').open('w') as telemetry,(folder/'thread-names.jsonl').open('w') as threads:
                process=subprocess.Popen(invocation,env=env,stdout=stdout,stderr=stderr,start_new_session=True)
                deadline=time.monotonic()+180
                while True:
                    tick=runner.telemetry(process.pid);telemetry.write(json.dumps(tick,sort_keys=True)+'\n');telemetry.flush()
                    if (folder/'manifest.json').exists():
                        try:
                            pid=json.loads((folder/'manifest.json').read_text())['pid'];names={}
                            for task in Path('/proc/'+str(pid)+'/task').iterdir():
                                try:names[task.name]=(task/'comm').read_text().strip()
                                except (FileNotFoundError,ProcessLookupError):pass
                            threads.write(json.dumps(dict(utc=runner.utc(),pid=pid,names=names),sort_keys=True)+'\n');threads.flush()
                        except (FileNotFoundError,ProcessLookupError,json.JSONDecodeError):pass
                    remaining=deadline-time.monotonic()
                    if remaining<=0:raise TimeoutError('diagnostic timeout: '+instrument+' '+arm)
                    try:process.wait(timeout=min(1,remaining));break
                    except subprocess.TimeoutExpired:pass
            status=process.returncode;runner.stop_process_group(process);process=None
            runner.snapshot(folder/'after')
            runner.dump(folder/'result.json',dict(run_id=entry['run_id'],returncode=status,started_utc=started,ended_utc=runner.utc(),process_wall_ns=time.monotonic_ns()-start))
            if status!=0:raise RuntimeError('diagnostic process failed: '+instrument+' '+arm)
            runner.validate_raw_output(folder,entry['workload']);runner.validate_identity(folder,entry,plan,pins)
            summary=json.loads((folder/'summary.json').read_text());assert summary['retries']==summary['warmup_retries']==0
            if instrument=='strace-summary':assert (folder/'strace-summary.txt').stat().st_size>0
            elif instrument=='perf-stat':assert (folder/'perf-stat.csv').stat().st_size>0
            else:assert list(folder.glob('trace.[0-9]*'))
            completed+=1;print(instrument+' '+arm+' validated',flush=True)
    for path,digest in pins.items():assert runner.sha(path)==digest,path
    assert completed+skipped==6
    runner.dump(out/'complete.json',dict(completed=completed,unavailable_perf_processes=skipped,scope='diagnostic counts only; no instrumented timing comparison'))
except BaseException as error:
    runner.dump(out/'ABORTED.json',dict(error=repr(error)));raise
finally:runner.stop_process_group(process)
