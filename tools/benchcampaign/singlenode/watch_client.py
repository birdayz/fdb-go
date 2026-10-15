#!/usr/bin/env python3
"""Controller-only client analysis watcher. No network, VM or benchmark commands.

--phase A --raw ROOT/results/client --output ROOT/analysis/client --protocol FILE
    --scripts-dir DIR --analyze-sha256 SHA --run-sha256 SHA [--remote-root DIR] [--once]
--scripts-dir is the controller's copy of the frozen tools/clientbench/scripts;
its analyze.py/run.py must hash to the given SHA-256s and to the plan pins at
--remote-root (the VM's FDB_BENCH_ROOT; default FDB_BENCH_ROOT, else /srv/fdb-bench).
Raw completed cases are copied to phase/workload/{before,cgo}; frozen analyzer
writes only those copies. Existing outputs refuse reuse. Adverse cases are N/A.
Root may write RAW/PHASE/phase-end.json when its orchestration future ends.
"""
import argparse
import csv
import importlib.util
import json
import math
import os
from pathlib import Path
import random
import shutil
import statistics
import subprocess
import sys
import time

# Set by configure() from the command line.
SCRIPTS = None
REMOTE_ROOT = None
PIN_ROOT = None
EXPECTED = None
ADVERSE = ('ABORTED.json', 'ADAPTER_ADVERSE.json', 'ADAPTER_PREFLIGHT_ADVERSE.json', 'UNEXECUTED.json')
GENERATED = ('analysis.json', 'TABLES.md', 'absolute.csv', 'paired.csv', 'per-run.csv')
METRICS = ('mean_ns', 'p99_ns', 'go_alloc_bytes_per_op', 'go_mallocs_per_op', 'shutdown_process_cpu_ns_per_op')


def configure(scripts_dir, remote_root, analyze_sha256, run_sha256):
    global SCRIPTS, REMOTE_ROOT, PIN_ROOT, EXPECTED
    SCRIPTS = Path(scripts_dir)
    REMOTE_ROOT = remote_root.rstrip('/')
    PIN_ROOT = REMOTE_ROOT+'/tools/clientbench/scripts/'
    EXPECTED = {'analyze.py': analyze_sha256, 'run.py': run_sha256}


def dump(path, data):
    path.write_text(json.dumps(data, indent=2, sort_keys=True, allow_nan=False)+'\n')


def sha(path):
    import hashlib
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for part in iter(lambda: stream.read(1048576), b''):
            h.update(part)
    return h.hexdigest()


def code_guard(pins):
    actual = {name: sha(SCRIPTS/name) for name in EXPECTED}
    if actual != EXPECTED or any(pins.get(PIN_ROOT+name) != digest for name, digest in actual.items()):
        raise ValueError('local analyzer/run.py differ from frozen performance plan code pins')
    return actual


def frozen_analyzer():
    sys.path.insert(0, str(SCRIPTS))
    spec = importlib.util.spec_from_file_location('frozen_client_analysis', SCRIPTS/'analyze.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def adverse_files(case, schedule):
    paths = [case/name for name in ADVERSE]
    for run in schedule:
        paths += [case/run['run_id']/name for name in ADVERSE]
    return [p for p in paths if p.exists()]


def raw_gate(case, state, workload, protocol, protocol_sha):
    if not (case/'plan.json').exists():
        return None
    plan = json.loads((case/'plan.json').read_text())
    schedule = json.loads((case/'schedule.json').read_text()) if (case/'schedule.json').exists() else []
    adverse = adverse_files(case, schedule)
    case_state = state.get('cases', {}).get(workload, {})
    if adverse or case_state.get('status') not in (None, 'started', 'complete'):
        return dict(status='N/A', adverse_files=[str(p) for p in adverse], case_state=case_state)
    if case_state.get('status') != 'complete' or not (case/'complete.json').exists():
        return None
    if any((case/name).exists() for name in GENERATED):
        raise FileExistsError('raw campaign already contains analyzer outputs; refusing to treat copies as original raw')
    complete = json.loads((case/'complete.json').read_text())
    pins = plan['expected_sha256']
    phase = next(p for p in protocol['phases'] if p['id'] == state['phase'])
    if plan['workloads'] != [dict(name=workload, workers=phase['workers'], iterations=phase['iterations'], warmup=phase['warmup'])] or pins.get(REMOTE_ROOT+'/protocol.json') != protocol_sha:
        raise ValueError('raw case is not the exact frozen performance population/protocol')
    code = code_guard(pins)
    runs = phase['blocks']*len(phase['arms'])
    if complete['completed'] != runs or complete['scheduled'] != runs or len(schedule) != runs or complete['binaries'] != pins:
        raise ValueError('incomplete or mismatched raw client population')
    if json.loads((case/'adapter-pins.finally.json').read_text()) != pins:
        raise ValueError('adapter final input pins changed')
    adapter = json.loads((case/'adapter.json').read_text())
    if adapter['plan_sha256'] != sha(case/'plan.json') or adapter['harness_revision'] != protocol['harness_revision']:
        raise ValueError('raw plan/harness identity changed')
    core = [case/name for name in ('plan.json', 'schedule.json', 'complete.json', 'binaries.json', 'adapter.json', 'adapter-pins.finally.json')]
    for run in schedule:
        folder = case/run['run_id']
        required = [folder/name for name in ('samples.csv', 'warmup.csv', 'summary.json', 'manifest.json', 'result.json', 'adapter-guard.json')]
        if not all(p.exists() for p in required):
            return None  # Transport in progress, never a zero-population pass.
        if json.loads((folder/'adapter-guard.json').read_text()).get('passed') is not True:
            raise ValueError('missing successful during-arm safety guard')
        core.extend(required)
    return dict(status='ready', plan=plan, schedule=schedule, code=code, runs=runs,
                core_hashes={str(p.relative_to(case)): sha(p) for p in core})


def supplement_allocations(analyzer, report, denominator):
    rows = report['per_run']
    stats = {(r['block'], r['arm']): r for r in rows}
    blocks = sorted({r['block'] for r in rows})
    rng = random.Random(391706)
    result = {}
    for metric in ('go_alloc_bytes_per_op', 'go_mallocs_per_op'):
        pairs = [(stats[b, 'final'][metric], stats[b, denominator][metric]) for b in blocks]
        if any(v == 0 for pair in pairs for v in pair):
            result[metric] = dict(status='undefined-zero', reason='absolute zero counters retained; no subset ratio')
        else:
            value = analyzer.bootstrap_log_ratio(pairs, rng, 20000)
            value.update(analyzer.dependence_sensitivity(pairs, rng, 20000))
            value.update(status='ok', numerator='final', denominator=denominator,
                         note='Go allocations only; excludes native libfdb_c heap', block_pairs=pairs)
            result[metric] = value
    return result


def analyze_case(case, destination, gate, workload, phase):
    if destination.exists():
        raise FileExistsError('refusing existing workload analysis output: '+str(destination))
    destination.mkdir(parents=True, exist_ok=False)
    if gate['status'] == 'N/A':
        shutil.copytree(case, destination/'adverse-raw')
        dump(destination/'N_A.json', gate)
        print(json.dumps(dict(phase=phase, workload=workload, status='N/A', reason=gate)), flush=True)
        return gate
    analyzer = frozen_analyzer()
    summary = dict(phase=phase, workload=workload, status='complete', code_sha256=gate['code'], denominators={},
                   scope=analyzer.LIFECYCLE_CPU_SCOPE,
                   inference='Nominal exploratory95% paired-block intervals, seed391706/20000; no aggregate/default-flip/capacity claim. Native Go allocations exclude C++ heap.')
    for denominator in ('before', 'cgo'):
        target = destination/denominator
        shutil.copytree(case, target)
        # No source files or raw originals are ever modified. The analyzer is
        # invoked as frozen, with explicit separate denominator/seed defaults.
        with open(destination/(denominator+'-analyzer.stdout'), 'x') as stdout, open(destination/(denominator+'-analyzer.stderr'), 'x') as stderr:
            subprocess.run([sys.executable, '-B', str(SCRIPTS/'analyze.py'), str(target), '--denominator', denominator,
                            '--seed', '391706', '--resamples', '20000'], stdout=stdout, stderr=stderr, check=True)
        report = json.loads((target/'analysis.json').read_text())
        if not report['valid_zero_error_campaign'] or len(report['per_run']) != gate['runs']:
            raise ValueError('frozen analyzer did not validate the exact zero-error run population')
        values = {c['metric']: c for c in report['comparisons'] if c['numerator'] == 'final' and c['denominator'] == denominator and c['metric'] in METRICS}
        values.update(supplement_allocations(analyzer, report, denominator))
        if not all(m in values for m in METRICS):
            raise ValueError('required report metric population missing')
        dump(target/'allocation-supplement.json', {m: values[m] for m in ('go_alloc_bytes_per_op', 'go_mallocs_per_op')})
        summary['denominators'][denominator] = values
    if {p: sha(case/p) for p in gate['core_hashes']} != gate['core_hashes']:
        raise ValueError('raw campaign changed during local analysis')
    code_guard(gate['plan']['expected_sha256'])
    dump(destination/'analysis-provenance.json', dict(watcher_sha256=sha(__file__), analyzer_code_sha256=gate['code'], raw_core_sha256=gate['core_hashes']))
    dump(destination/'summary.json', summary)
    print(json.dumps(summary), flush=True)
    return summary


def watch(args):
    root, output = Path(args.raw)/args.phase, Path(args.output)/args.phase
    protocol = json.loads(Path(args.protocol).read_text())
    phase = next(p for p in protocol['phases'] if p['id'] == args.phase)
    workloads = phase['workloads']
    handled = {}
    statefile = Path(args.output)/('watch-'+args.phase+'.json')
    if statefile.exists():
        raise FileExistsError('refusing existing watcher state')
    # A controller may finish an initial case before starting this watcher.
    # Read completed summaries, never rerun or overwrite existing analyses.
    if output.exists():
        for workload in workloads:
            folder = output/workload
            if not folder.exists():
                continue
            receipt = folder/'summary.json' if (folder/'summary.json').exists() else folder/'N_A.json'
            if not receipt.exists():
                raise FileExistsError('existing incomplete analysis output: '+str(folder))
            handled[workload] = json.loads(receipt.read_text())
    output.mkdir(parents=True, exist_ok=True)
    stopped = False
    while True:
        state_path = root/'client-phase-state.json'
        try:
            state = json.loads(state_path.read_text()) if state_path.exists() else {}
        except json.JSONDecodeError:
            time.sleep(args.poll_seconds)
            continue  # Await an atomic/complete controller transfer, not a measurement retry.
        for workload in workloads:
            if workload in handled:
                continue
            case = root/workload
            if not case.exists():
                continue
            gate = raw_gate(case, state, workload, protocol, sha(args.protocol))
            if gate:
                handled[workload] = analyze_case(case, output/workload, gate, workload, args.phase)
                dump(statefile, dict(phase=args.phase, handled=handled, expected_cases=workloads, status='watching'))
        end = state.get('final_failure') or any((root/name).exists() for name in ('phase-end.json', 'client-phase-complete.json', 'client-phase-unexecuted.json'))
        if len(handled) == len(workloads) or end or args.once:
            dump(statefile, dict(phase=args.phase, handled=handled, expected_cases=workloads,
                                not_available=[w for w in workloads if w not in handled], status='stopped', phase_end_observed=bool(end)))
            return
        time.sleep(args.poll_seconds)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--phase', default='A', choices=['A', 'A2'])
    p.add_argument('--raw', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--protocol', required=True)
    p.add_argument('--poll-seconds', type=float, default=15)
    p.add_argument('--once', action='store_true')
    p.add_argument('--scripts-dir', required=True, help="controller copy of the frozen tools/clientbench/scripts")
    p.add_argument('--analyze-sha256', required=True, help='frozen analyze.py SHA-256')
    p.add_argument('--run-sha256', required=True, help='frozen run.py SHA-256')
    p.add_argument('--remote-root', default=os.environ.get('FDB_BENCH_ROOT', '/srv/fdb-bench'),
                   help="the VM's FDB_BENCH_ROOT, as recorded in plan pins")
    args = p.parse_args()
    configure(args.scripts_dir, args.remote_root, args.analyze_sha256, args.run_sha256)
    watch(args)


if __name__ == '__main__':
    main()
