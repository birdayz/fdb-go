#!/usr/bin/env python3
"""Seal the multi-node campaign: frozen arm orders and per-phase plans. Runs nothing.

orders          protocol.json -> orders.json (exclusive create). Every phase gets
                orders from random.Random(order_seed): all arm permutations,
                repeated blocks/n! times, shuffled once. This is exactly the
                order tools/clientbench/scripts/run.py derives for a
                one-workload plan, so client schedules can be checked against it.
check-orders    regenerate and compare; exit 1 on any difference.
client-plans    M1 phases: one clientbench plan per (phase, workload) through the
                frozen matrix_plan.make_plan, then run.make_schedule (both
                imported from --scripts-dir), checked block by block against
                orders.json. `workers` is set afterwards as a data override:
                matrix_plan always writes workers=1. matrix_plan's campaign wall
                cap defaults to 180 s and run.py refuses more than 900 s; the
                protocol's per-workload cap must lie in (waits, 900].
testingb-plans  M2 phases: one job list per phase (cell-major, then block,
                then frozen position), testing.B argv with the pinned binary,
                exact sub-benchmark regex, -test.count/-test.benchtime, and the
                placement preset/cell each case must satisfy.

Workloads/cells a phase lists under new_harness_* do not exist in the frozen
harness; they are left out (and listed as unsealed in the manifest) unless
named with --allow-new-harness, which root may only pass after the separately
reviewed harness commit is built and its binaries are the ones pinned here.
"""
import argparse
import glob
import hashlib
import importlib
import itertools
import json
import math
from pathlib import Path
import random
import re
import sys

import mn_common as mc

CLIENT_ARMS = {  # protocol arm -> (plan arm name, binary key, production revision key)
    'before-Go': ('before', 'before', 'before_allocation_work_after_read_parity'),
    'final-Go': ('final', 'final', 'final'),
    'CGo-7.3.77': ('cgo', 'cgo', 'final'),
}
TESTINGB_ARMS = {'final-ordinary': 'SCAN_AND_FETCH', 'final-remote': 'USE_REMOTE_FETCH'}
MAX_CAMPAIGN_WALL_CAP_SECONDS = 900  # run.py's hard maximum
WAIT_SECONDS_PER_RUN = 3  # matrix_plan cooldown 2 + quiescence 1


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


# --- orders ------------------------------------------------------------------

def phase_orders(phase):
    arms, blocks = phase['arms'], phase['blocks']
    n = len(arms)
    if n < 2 or len(set(arms)) != n:
        raise ValueError(f'{phase["id"]}: two or more distinct arms required')
    if blocks < 2 or blocks % math.factorial(n):
        raise ValueError(f'{phase["id"]}: blocks must be a positive multiple of {math.factorial(n)}')
    rng = random.Random(phase['order_seed'])
    orders = list(itertools.permutations(range(n))) * (blocks // math.factorial(n))
    rng.shuffle(orders)
    return [[arms[i] for i in o] for o in orders]


def check_balance(phase, orders):
    arms = phase['arms']
    if len(orders) != phase['blocks'] or any(sorted(o) != sorted(arms) for o in orders):
        raise ValueError(f'{phase["id"]}: every block must run every arm once')
    counts = {(a, p): sum(o[p] == a for o in orders) for a in arms for p in range(len(arms))}
    if len(set(counts.values())) != 1:
        raise ValueError(f'{phase["id"]}: arm positions are not balanced')


def make_orders(protocol):
    seeds = [p['order_seed'] for p in protocol['phases']]
    if len(set(seeds)) != len(seeds):
        raise ValueError('phase order seeds must be distinct')
    out = {}
    for phase in protocol['phases']:
        orders = phase_orders(phase)
        check_balance(phase, orders)
        out[phase['frozen_order_key']] = dict(seed=phase['order_seed'], arms=phase['arms'], orders=orders)
    return out


# --- client (M1) plans ---------------------------------------------------------

def campaign_id(campaign, phase, workload):
    return hashlib.sha256(f'{campaign}/{phase}/{workload}'.encode()).hexdigest()[:29]


def sealed_items(phase, key, allowed):
    """(sealed, unsealed) items of phase[key]; new-harness items need explicit approval."""
    new = set(phase.get('new_harness_' + key, []))
    sealed = [w for w in phase[key] if w not in new or w in allowed]
    return sealed, [w for w in phase[key] if w not in sealed]


def load_frozen(scripts_dir):
    """Import matrix_plan and run from exactly scripts_dir, never a cached copy."""
    for name in ('matrix_plan', 'run', 'r3_plan'):
        sys.modules.pop(name, None)
    sys.path.insert(0, str(Path(scripts_dir).resolve()))
    try:
        modules = importlib.import_module('matrix_plan'), importlib.import_module('run')
    finally:
        sys.path.pop(0)
    for m in modules:
        # Compare directories, not resolved files: Bazel runfiles are symlinks.
        if Path(m.__file__).absolute().parent.resolve() != Path(scripts_dir).resolve():
            raise ValueError(f'{m.__name__} was not loaded from {scripts_dir}')
    return modules


def client_plan(args, protocol, phase, workload, matrix_plan, pinned):
    runtime = protocol['runtime']
    client = runtime['client_vm']
    waits = phase['blocks'] * len(phase['arms']) * WAIT_SECONDS_PER_RUN
    cap = phase['per_workload_campaign_wall_cap_seconds']
    if not waits < cap <= MAX_CAMPAIGN_WALL_CAP_SECONDS:
        raise ValueError(f'{phase["id"]}: cap {cap}s must exceed {waits}s of waits and be at most 900s')
    arms = []
    for name in phase['arms']:
        plan_name, binary_key, revision_key = CLIENT_ARMS[name]
        arms.append(f'{plan_name}={args.binaries[binary_key]}@{protocol["production_revisions"][revision_key]}')
    ns = argparse.Namespace(
        arm=arms, workload=[workload], iterations=phase['iterations'], warmup=phase['warmup'],
        blocks=phase['blocks'], cluster_file=args.cluster_file, native_library=args.native_library,
        server_binary=args.server_binary, harness_id=sha256(args.harness_archive), pin=[str(p) for p in pinned],
        campaign_id=campaign_id(protocol['campaign'], phase['id'], workload), seed=phase['order_seed'],
        gomaxprocs=runtime['GOMAXPROCS'], housekeeping_cpus=client['housekeeping_cpus'],
        server_cpus=client['idle_sentinel_cpus'], client_cpus=client['client_cpus'],
        campaign_wall_cap_seconds=cap, timeout_seconds=phase['per_process_timeout_seconds'])
    plan = matrix_plan.make_plan(ns)
    for w in plan['workloads']:
        w['workers'] = phase['workers']  # the only data override
    return plan


def check_schedule(schedule, phase, frozen):
    by_plan = {CLIENT_ARMS[a][0]: a for a in phase['arms']}
    for block in range(phase['blocks']):
        runs = sorted((r for r in schedule if r['block'] == block), key=lambda r: r['position'])
        if [by_plan[r['arm']] for r in runs] != frozen[block]:
            raise ValueError(f'{phase["id"]} block {block}: run.py order differs from orders.json')
    if len(schedule) != phase['blocks'] * len(phase['arms']):
        raise ValueError(f'{phase["id"]}: incomplete schedule')


def pins(paths, pin_globs):
    out = [Path(p) for p in paths]
    for pattern in pin_globs:
        matched = sorted(glob.glob(pattern))
        if not matched:
            raise ValueError('pin glob matched nothing: ' + pattern)
        out += [Path(m) for m in matched]
    resolved = sorted({p.resolve() for p in out})
    for p in resolved:
        if not p.is_file():
            raise ValueError(f'pinned path is not a regular file: {p}')
    return resolved


def seal_client(args):
    protocol = json.loads(Path(args.protocol).read_text())
    orders = json.loads(Path(args.orders).read_text())
    if orders != make_orders(protocol):
        raise ValueError('orders.json differs from the protocol seeds; reseal orders first')
    matrix_plan, run = load_frozen(args.scripts_dir)
    pinned = pins([args.protocol, args.orders, args.harness_archive, *args.pin], args.pin_glob)
    out = Path(args.output_dir)
    out.mkdir(parents=True, exist_ok=False)
    manifest = dict(campaign=protocol['campaign'], protocol_sha256=sha256(args.protocol),
                    orders_sha256=sha256(args.orders), generator_sha256=sha256(__file__),
                    scripts_dir=str(Path(args.scripts_dir).resolve()),
                    pinned_files={str(p): sha256(p) for p in pinned}, campaigns=[], unsealed=[])
    for phase in protocol['phases']:
        if phase['kind'] != 'client' or (args.phase and phase['id'] not in args.phase):
            continue
        frozen = orders[phase['frozen_order_key']]['orders']
        sealed, unsealed = sealed_items(phase, 'workloads', set(args.allow_new_harness))
        manifest['unsealed'] += [dict(phase=phase['id'], workload=w, reason='requires reviewed new harness commit')
                                 for w in unsealed]
        for workload in sealed:
            plan = client_plan(args, protocol, phase, workload, matrix_plan, pinned)
            folder = out / phase['id'] / workload
            folder.mkdir(parents=True)
            schedule = run.make_schedule(plan, Path(args.results_dir) / phase['id'] / workload)
            check_schedule(schedule, phase, frozen)
            mc.dump_json(folder / 'plan.json', plan)
            mc.dump_json(folder / 'schedule.json', schedule)
            manifest['campaigns'].append(dict(
                phase=phase['id'], workload=workload, workers=phase['workers'], iterations=phase['iterations'],
                warmup=phase['warmup'], blocks=phase['blocks'], runs=len(schedule), campaign=plan['campaign'],
                campaign_wall_cap_seconds=plan['campaign_wall_cap_seconds'], timeout_seconds=plan['timeout_seconds'],
                plan=str(folder / 'plan.json'), plan_sha256=sha256(folder / 'plan.json'),
                run=['python3', '-B', str(Path(args.scripts_dir).resolve() / 'run.py'), '--plan',
                     str(folder / 'plan.json'), '--output', str(Path(args.results_dir) / phase['id'] / workload)]))
    mc.dump_json(out / 'manifest.json', manifest)
    return manifest


# --- testing.B (M2) plans --------------------------------------------------------

def testingb_cells(phase):
    """Expand a phase's compact cell spec into cases."""
    kind = phase['binary_kind']
    cases = []
    if kind == 'fetch':
        for payload, cell, direction in itertools.product(phase['payloads'], phase['cells'], phase['dirs']):
            placement = f'{cell}-{direction}' if cell == 'limit1' else cell
            cases.append(dict(key=f'{payload}-{cell}-{direction}', cell=cell, payload=payload, dir=direction,
                              stem=f'BenchmarkIndexFetch/payload={payload}/cell={cell}/dir={direction}',
                              store=f'benchfetch/{payload}', placement_preset='benchfetch',
                              placement_cell=placement, chunks=1 if payload == 'small' else 2))
    elif kind == 'planskip':
        for cell, direction in itertools.product(phase['cells'], phase['dirs']):
            cases.append(dict(key=f'skip3-{cell}-{direction}', cell=cell, payload='small', dir=direction,
                              stem=f'BenchmarkPlanSkip/cell={cell}/dir={direction}', store='benchfetch/planskip',
                              placement_preset='planskip', placement_cell=f'skip3-{cell}', chunks=1))
    elif kind == 'sqlfetch':
        for cell in phase['cells']:
            cases.append(dict(key=f'sql-{cell}', cell=cell, payload='sql', dir='forward',
                              stem=f'BenchmarkSQLIndexFetch/query={cell}', store='sql/IDX_A',
                              placement_preset='sql', placement_cell=cell, chunks=None))
    else:
        raise ValueError('unknown binary kind ' + kind)
    return cases


def bench_regex(name):
    # testing.B matches each slash-separated element as its own regex.
    return '/'.join('^' + re.escape(part) + '$' for part in name.split('/'))


def testingb_plan(protocol, phase, frozen, binary, cluster_file, allowed):
    client = protocol['runtime']['client_vm']
    if set(phase['arms']) - set(TESTINGB_ARMS):
        raise ValueError(f'{phase["id"]}: unknown testing.B arms')
    cases, unsealed = [], []
    new = set(phase.get('new_harness_cells', []))
    whole_phase_new = phase.get('new_harness_phase', False)
    for case in testingb_cells(phase):
        needs_new = whole_phase_new or case['cell'] in new
        if needs_new and not ({case['cell'], case['key'], phase['id']} & allowed):
            unsealed.append(dict(phase=phase['id'], case=case['key'], reason='requires reviewed new harness commit'))
        else:
            cases.append(case)
    jobs = []
    for ci, case in enumerate(cases):
        for block, order in enumerate(frozen):
            for position, arm in enumerate(order):
                name = f'{case["stem"]}/method={TESTINGB_ARMS[arm]}'
                command = ['taskset', '--cpu-list', client['client_cpus'], binary, '-test.run=^$', '-test.v',
                           '-test.bench=' + bench_regex(name), f'-test.count={phase["repeats_per_arm_process"]}',
                           '-test.benchmem', '-test.benchtime=' + phase['benchtime'], '-test.timeout=0']
                jobs.append(dict(id=f'{phase["id"]}-c{ci:02d}-b{block:02d}-p{position}-{arm}', case=case['key'],
                                 block=block, position=position, arm=arm, name=name, command=command))
    if len({j['id'] for j in jobs}) != len(jobs):
        raise ValueError('duplicate job IDs')
    env = dict(GOMAXPROCS=str(protocol['runtime']['GOMAXPROCS']), GOGC='100', GOMEMLIMIT='off', GODEBUG='',
               TZ='UTC', LC_ALL='C', FDB_BENCH_CLUSTER_FILE=cluster_file, FDB_CLUSTER_FILE=cluster_file)
    return dict(version=1, campaign=protocol['campaign'], phase=phase['id'], engine=phase['engine'],
                cases=cases, jobs=jobs, orders=frozen, env=env, process_timeout_seconds=phase['process_timeout_seconds'],
                phase_wall_cap_seconds=phase['wall_cap_seconds'], placement_required=phase['placement_required']), unsealed


def seal_testingb(args):
    protocol = json.loads(Path(args.protocol).read_text())
    orders = json.loads(Path(args.orders).read_text())
    if orders != make_orders(protocol):
        raise ValueError('orders.json differs from the protocol seeds; reseal orders first')
    out = Path(args.output_dir)
    out.mkdir(parents=True, exist_ok=False)
    pinned = pins([args.protocol, args.orders, args.cluster_file, *args.binaries.values(), *args.pin], args.pin_glob)
    manifest = dict(campaign=protocol['campaign'], generator_sha256=sha256(__file__),
                    pinned_files={str(p): sha256(p) for p in pinned}, phases=[], unsealed=[])
    for phase in protocol['phases']:
        if phase['kind'] != 'testingb' or (args.phase and phase['id'] not in args.phase):
            continue
        if phase.get('optional') and phase['id'] not in (args.phase or []):
            continue
        binary = str(Path(args.binaries[phase['binary_kind']]).resolve())
        plan, unsealed = testingb_plan(protocol, phase, orders[phase['frozen_order_key']]['orders'], binary,
                                       str(Path(args.cluster_file).resolve()), set(args.allow_new_harness))
        plan['expected_sha256'] = {binary: sha256(binary), str(Path(args.cluster_file).resolve()): sha256(args.cluster_file)}
        manifest['unsealed'] += unsealed
        if not plan['jobs']:
            continue
        mc.dump_json(out / f'{phase["id"]}.plan.json', plan)
        manifest['phases'].append(dict(phase=phase['id'], cases=len(plan['cases']), jobs=len(plan['jobs']),
                                       plan_sha256=sha256(out / f'{phase["id"]}.plan.json')))
    mc.dump_json(out / 'manifest.json', manifest)
    return manifest


def parse_binaries(values):
    out = {}
    for v in values:
        k, sep, path = v.partition('=')
        if not sep or not k or not path or k in out:
            raise ValueError('binary must be unique KIND=PATH: ' + v)
        out[k] = path
    return out


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='command', required=True)
    o = sub.add_parser('orders')
    o.add_argument('--protocol', required=True)
    o.add_argument('--output', required=True)
    co = sub.add_parser('check-orders')
    co.add_argument('--protocol', required=True)
    co.add_argument('--orders', required=True)
    for name in ('client-plans', 'testingb-plans'):
        p = sub.add_parser(name)
        p.add_argument('--protocol', required=True)
        p.add_argument('--orders', required=True)
        p.add_argument('--binary', action='append', default=[],
                       help='client: final=, cgo=, before=PATH; testing.B: fetch=, planskip=, sqlfetch=PATH')
        p.add_argument('--cluster-file', required=True)
        p.add_argument('--pin', action='append', default=[])
        p.add_argument('--pin-glob', action='append', default=[])
        p.add_argument('--phase', action='append', default=[])
        p.add_argument('--allow-new-harness', action='append', default=[],
                       help='workload/cell/phase approved after the reviewed harness rebuild')
        p.add_argument('--output-dir', required=True)
        if name == 'client-plans':
            p.add_argument('--scripts-dir', required=True, help='the frozen tools/clientbench/scripts')
            p.add_argument('--native-library', required=True)
            p.add_argument('--server-binary', required=True)
            p.add_argument('--harness-archive', required=True)
            p.add_argument('--results-dir', required=True)
    args = ap.parse_args(argv)
    if args.command == 'orders':
        orders = make_orders(json.loads(Path(args.protocol).read_text()))
        mc.dump_json(args.output, orders)
        print(f'{len(orders)} phase order sets written to {args.output}')
        return 0
    if args.command == 'check-orders':
        same = json.loads(Path(args.orders).read_text()) == make_orders(json.loads(Path(args.protocol).read_text()))
        print('orders match protocol seeds' if same else 'ORDERS DIFFER from protocol seeds')
        return 0 if same else 1
    args.binaries = parse_binaries(args.binary)
    manifest = seal_client(args) if args.command == 'client-plans' else seal_testingb(args)
    print(json.dumps(dict(sealed=len(manifest.get('campaigns', manifest.get('phases', []))),
                          unsealed=len(manifest['unsealed'])), sort_keys=True))
    return 0


if __name__ == '__main__':
    sys.exit(main())
