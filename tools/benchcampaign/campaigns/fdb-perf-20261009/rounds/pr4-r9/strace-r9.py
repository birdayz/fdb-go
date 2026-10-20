#!/usr/bin/env python3
"""Exactly one instrumented read1000 process per Go arm, after timed campaigns."""
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import time

spec = importlib.util.spec_from_file_location('runner', '/opt/fdbperf/scripts/astra-perf-run.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
base = Path('/opt/fdbperf/artifacts/pr4-r9-build')
source = Path('/opt/fdbperf/artifacts/pr4-r9-read1000')
plan = json.loads((source/'plan.json').read_text())
schedule = json.loads((source/'schedule.json').read_text())
pins = json.loads((source/'binaries.json').read_text())
out = Path('/opt/fdbperf/artifacts/pr4-r9-strace')
out.mkdir(exist_ok=False)
runner.dump(out/'scope.json', dict(instrumentation='strace -f -c, one process per old/new Go arm, NOT timed campaign data', measured=5000, warmup=200,
    normalization='all process-lifetime syscalls divided by5000 measured transactions, INCLUDING setup/warmup/validation/runtime/hash reads; NOT measured-phase syscall counts',
    strace_sha256=runner.sha('/usr/bin/strace'), old_new_order='old then new; diagnostic, not a counterbalanced latency comparison'))
p = None
for sig in (signal.SIGTERM, signal.SIGINT):
    signal.signal(sig, runner.interrupted)
try:
    for arm in ['old', 'new']:
        runner.check_builds()
        entry = next(r for r in schedule if r['arm'] == arm and r['block'] == 0)
        folder = out/arm
        folder.mkdir()
        time.sleep(2)
        gate = runner.quiescence(plan)
        runner.dump(folder/'quiescence.json', gate)
        if not gate['passed']:
            raise RuntimeError('diagnostic quiescence failed: '+arm)
        command = list(entry['command'])
        for flag, value in [('--output',str(folder)),('--run-id','strace-'+arm),('--prefix','clientbench/r9d1/xxxxxxxxxxxxxxxxxxxxxxxx/read1000________/b000/'+arm+'/')]:
            command[command.index(flag)+1] = value
        binary = next(a['binary'] for a in plan['arms'] if a['name'] == arm)
        if runner.sha(binary) != pins[binary]:
            raise RuntimeError('binary changed before strace')
        invocation = ['/usr/bin/time','-v','-o',str(folder/'process-time.txt'),'--','/usr/bin/strace','-f','-c','-o',str(folder/'strace-summary.txt'),'--'] + command
        runner.dump(folder/'invocation.json', invocation)
        env = {'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin','HOME':str(Path.home()),**plan['environment']}
        runner.snapshot(folder/'before')
        started = runner.utc()
        start = time.monotonic_ns()
        with (folder/'stdout.txt').open('w') as stdout, (folder/'stderr.txt').open('w') as stderr, (folder/'telemetry.jsonl').open('w') as telemetry:
            p = subprocess.Popen(invocation, env=env, stdout=stdout, stderr=stderr, start_new_session=True)
            deadline = time.monotonic() + 180
            while True:
                telemetry.write(json.dumps(runner.telemetry(p.pid),sort_keys=True)+'\n')
                telemetry.flush()
                remaining = deadline-time.monotonic()
                if remaining <= 0:
                    raise TimeoutError('strace diagnostic timeout')
                try:
                    p.wait(timeout=min(1, remaining))
                    break
                except subprocess.TimeoutExpired:
                    pass
        code = p.returncode
        runner.stop_process_group(p)
        p = None
        runner.snapshot(folder/'after')
        runner.dump(folder/'result.json', dict(returncode=code,started_utc=started,ended_utc=runner.utc(),process_wall_ns=time.monotonic_ns()-start))
        if code != 0:
            raise RuntimeError('strace process failed: '+arm)
        runner.validate_raw_output(folder, plan['workloads'][0])
        manifest = json.loads((folder/'manifest.json').read_text())
        if manifest['backend']['executable_sha256'] != pins[binary] or runner.sha(binary) != pins[binary]:
            raise RuntimeError('diagnostic binary identity mismatch')
        if manifest['backend']['native_library'] or manifest['config']['profile'] != 'none':
            raise RuntimeError('unexpected diagnostic backend/profile')
        print(arm+' strace validated',flush=True)
    runner.dump(out/'complete.json',dict(completed=2,scope='diagnostic syscall counts only; no timing comparison'))
except BaseException as e:
    runner.dump(out/'ABORTED.json',dict(error=repr(e)))
    raise
finally:
    runner.stop_process_group(p)
