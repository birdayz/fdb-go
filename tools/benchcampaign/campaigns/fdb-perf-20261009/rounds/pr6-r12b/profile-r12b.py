#!/usr/bin/env python3
"""Four prespecified, serial attribution runs; no timing inference."""
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
source = Path('/opt/fdbperf/artifacts/pr6-r12-batchset50')
plan = json.loads((source/'plan.json').read_text())
pins = json.loads((source/'binaries.json').read_text())
schedule = json.loads((source/'schedule.json').read_text())
for campaign in ['pr6-r12-batchset50', 'pr6-r12-set100', 'pr6-r12b-batchset50']:
    done = json.loads((Path('/opt/fdbperf/artifacts')/campaign/'complete.json').read_text())
    assert done['completed'] == done['scheduled'] == 18, campaign
assert sorted(os.sched_getaffinity(0)) == [0, 1]
for path, digest in pins.items():
    assert runner.sha(path) == digest, path
out = Path('/opt/fdbperf/artifacts/pr6-r12b-profile')
out.mkdir(exist_ok=False)
runs = [dict(arm=arm, workload='batchset50', iterations=3000, warmup=100, profile='cpu', instrument='pprof') for arm in ['old','new','new','old']]
runner.dump(out/'plan.json', dict(runs=runs, environment=plan['environment'], cpu_sets=plan['cpu_sets'],
    scope='Attribution only. CPU pprof wraps measured loop. Heap profile is sampled Go heap after forced GC; alloc_space includes process lifetime including setup/warmup, not native C++ heap. perf record covers whole process. No timings or cross-arm causal cost claims.',
    perf_argv=['/usr/bin/perf','record','-F','1999','-g','--call-graph','fp'],
    pin_source=str(source), original_plan=plan, per_run_timeout_seconds=180, python_affinity=sorted(os.sched_getaffinity(0))))
runner.dump(out/'binaries.json', pins)
runner.dump(out/'tools.json', {path:runner.sha(path) for path in [__file__, '/usr/bin/perf', '/usr/bin/time', '/usr/bin/taskset']})
p = None
for sig in (signal.SIGTERM, signal.SIGINT):
    signal.signal(sig, runner.interrupted)
try:
    for index, run in enumerate(runs):
        arm, work = run['arm'], run['workload']
        runner.check_builds()
        for path, digest in pins.items():
            assert runner.sha(path) == digest, path
        entry = next(r for r in schedule if r['arm'] == arm and r['block'] == 0)
        name = f"{index:02d}-{arm}-{work}-{run['instrument']}-{run['profile']}"
        folder = out/name
        folder.mkdir()
        time.sleep(2)
        gate = runner.quiescence(plan)
        runner.dump(folder/'quiescence.json', gate)
        if not gate['passed']:
            raise RuntimeError('attribution quiescence failed: '+name)
        command = list(entry['command'])
        prefix = f'clientbench/r18p/xxxxxxxxxxxxxxxxxxxxxxxx/{work:_<16}/b{index:03d}/{arm}/'
        assert len(prefix.encode()) == 68
        for flag,value in [('--workload',work),('--iterations',str(run['iterations'])),('--warmup',str(run['warmup'])),('--output',str(folder)),('--run-id',name),('--block',str(index)),('--prefix',prefix)]:
            command[command.index(flag)+1] = value
        command += ['--profile',run['profile']]
        binary = next(a['binary'] for a in plan['arms'] if a['name'] == arm)
        wrapper = []
        if run['instrument'] == 'perf':
            wrapper = ['/usr/bin/perf','record','-F','1999','-g','--call-graph','fp','-o',str(folder/'perf.data'),'--']
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
                    raise TimeoutError(name+' attribution timeout')
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
            raise RuntimeError(name+' process failed')
        workload = dict(name=work,iterations=run['iterations'],warmup=run['warmup'],workers=1)
        runner.validate_raw_output(folder, workload)
        manifest = json.loads((folder/'manifest.json').read_text())
        summary = json.loads((folder/'summary.json').read_text())
        assert summary['retries'] == summary['warmup_retries'] == 0
        assert manifest['backend']['executable_sha256'] == pins[binary] == runner.sha(binary)
        cfg = manifest['config']
        expected_revision = next(a['source_revision'] for a in plan['arms'] if a['name'] == arm)
        assert manifest['backend']['native_library'] == (arm == 'cgo')
        for key,value in dict(profile=run['profile'],source_revision=expected_revision,prefix=prefix,iterations=run['iterations'],warmup=run['warmup'],workers=1,harness_id=plan['harness_id']).items():
            assert cfg[key] == value, (key,cfg[key],value)
        libraries = manifest['backend']['loaded_fdb_libraries_sha256']
        assert bool(libraries) == (arm == 'cgo')
        for path,digest in libraries.items():
            assert digest == pins['/usr/lib/libfdb_c.so'] == runner.sha(path)
        profile_file = folder/('perf.data' if run['instrument']=='perf' else run['profile']+'.pprof')
        assert profile_file.stat().st_size > 0
        print(name+' validated',flush=True)
    for path,digest in pins.items():
        assert runner.sha(path) == digest, path
    runner.dump(out/'complete.json',dict(completed=len(runs),scope='attribution only; no timing comparison'))
except BaseException as error:
    runner.dump(out/'ABORTED.json',dict(error=repr(error)))
    raise
finally:
    runner.stop_process_group(p)
