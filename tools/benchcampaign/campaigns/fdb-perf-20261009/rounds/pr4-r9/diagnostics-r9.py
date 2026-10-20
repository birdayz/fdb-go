#!/usr/bin/env python3
"""One old/new read process per instrument, strictly after timed campaigns."""
import importlib.util
import json
from pathlib import Path
import signal
import subprocess
import time

spec = importlib.util.spec_from_file_location('runner', '/opt/fdbperf/scripts/astra-perf-run.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
source = Path('/opt/fdbperf/artifacts/pr4-r9-read1000')
plan = json.loads((source/'plan.json').read_text())
schedule = json.loads((source/'schedule.json').read_text())
pins = json.loads((source/'binaries.json').read_text())
for timed in ['read1000', 'batchset50']:
    completed = json.loads((Path('/opt/fdbperf/artifacts/pr4-r9-'+timed)/'complete.json').read_text())
    if completed['completed'] != completed['scheduled'] or completed['completed'] != 18:
        raise RuntimeError('timed campaign incomplete before diagnostics')
p = None
for sig in (signal.SIGTERM, signal.SIGINT):
    signal.signal(sig, runner.interrupted)
for instrument, code in [('perf', 'r9p1'), ('strace', 'r9d1')]:
    out = Path('/opt/fdbperf/artifacts/pr4-r9-'+instrument)
    out.mkdir(exist_ok=False)
    runner.dump(out/'scope.json', dict(instrument=instrument, measured=5000, warmup=200,
        process_population='one old then one new process; separate from timed campaigns and other instrumentation',
        normalization='whole-process counters divided by5000 measured transactions; includes startup, executable hashing, setup, warmup, measurement, validation and shutdown; NOT phase counts',
        executable_sha256=runner.sha('/usr/bin/'+instrument),
        warning='perf task-clock is not measured-phase getrusage; instrumented timing does not enter campaign conclusions'))
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
            prefix = 'clientbench/'+code+'/xxxxxxxxxxxxxxxxxxxxxxxx/read1000________/b000/'+arm+'/'
            if len(prefix.encode()) != 68:
                raise RuntimeError('diagnostic prefix width differs')
            for flag, value in [('--output',str(folder)),('--run-id',instrument+'-'+arm),('--prefix',prefix)]:
                command[command.index(flag)+1] = value
            binary = next(a['binary'] for a in plan['arms'] if a['name'] == arm)
            if runner.sha(binary) != pins[binary]:
                raise RuntimeError('binary changed before diagnostic')
            if instrument == 'perf':
                wrapper = ['/usr/bin/perf','stat','-x,','--no-big-num','-e','context-switches,task-clock','-o',str(folder/'perf-stat.csv'),'--']
            else:
                wrapper = ['/usr/bin/strace','-f','-c','-o',str(folder/'strace-summary.txt'),'--']
            invocation = ['/usr/bin/time','-v','-o',str(folder/'process-time.txt'),'--'] + wrapper + command
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
                        raise TimeoutError(instrument+' diagnostic timeout')
                    try:
                        p.wait(timeout=min(1, remaining))
                        break
                    except subprocess.TimeoutExpired:
                        pass
            status = p.returncode
            runner.stop_process_group(p)
            p = None
            runner.snapshot(folder/'after')
            runner.dump(folder/'result.json', dict(returncode=status,started_utc=started,ended_utc=runner.utc(),process_wall_ns=time.monotonic_ns()-start))
            if status != 0:
                raise RuntimeError(instrument+' process failed: '+arm)
            runner.validate_raw_output(folder, plan['workloads'][0])
            manifest = json.loads((folder/'manifest.json').read_text())
            if manifest['backend']['executable_sha256'] != pins[binary] or runner.sha(binary) != pins[binary]:
                raise RuntimeError('diagnostic binary identity mismatch')
            cfg = manifest['config']
            expected_revision = next(a['source_revision'] for a in plan['arms'] if a['name'] == arm)
            if (manifest['backend']['native_library'] or cfg['profile'] != 'none' or cfg['source_revision'] != expected_revision or
                    cfg['prefix'] != prefix or cfg['iterations'] != 5000 or cfg['warmup'] != 200 or cfg['harness_id'] != plan['harness_id']):
                raise RuntimeError('unexpected diagnostic identity/configuration')
            print(arm+' '+instrument+' validated',flush=True)
        runner.dump(out/'complete.json',dict(completed=2,scope='whole-process diagnostic counters only; no timing comparison'))
    except BaseException as e:
        runner.dump(out/'ABORTED.json',dict(error=repr(e)))
        raise
    finally:
        runner.stop_process_group(p)
