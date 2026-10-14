#!/usr/bin/env python3
"""Seal the client phases' (A, A2) per-workload clientbench plans on the VM.

Run on the benchmark VM, where every pinned path exists: plans hash the real
binaries, native library, server, cluster file, harness archive and receipts at
their absolute runtime paths. Nothing is executed against FoundationDB.

For each phase and workload this writes, under --output-dir/<phase>/<workload>/:
  plan.json      matrix_plan.make_plan for the three protocol arms (arm order =
                 protocol order, seed = the phase's order_seed, one workload),
                 with the protocol's workers count as the only data override
  schedule/      run.py --plan plan.json --output schedule --schedule-only
                 (the real CLI; its plan.json/schedule.json are what run.py uses)
It then checks every schedule against orders.json: block b runs the frozen
order b, and prefixes are unique and of equal length. --output-dir/manifest.json
records each file's SHA-256 and the exact run command per campaign. The
campaign name is protocol.json's "campaign". Path defaults live under
FDB_BENCH_ROOT (default /srv/fdb-bench); --netns defaults to FDB_BENCH_NETNS
(default fdb-bench-client).
"""
import argparse
import glob
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(os.environ.get('FDB_BENCH_ROOT', '/srv/fdb-bench'))
NETNS = os.environ.get('FDB_BENCH_NETNS', 'fdb-bench-client')
PHASES = ('A', 'A2')
# Protocol arm name -> (plan arm name, binary option, revision key).
ARMS = {
    'before-Go': ('before', 'before_binary', 'before_allocation_work_after_read_parity'),
    'final-Go': ('final', 'final_binary', 'final'),
    'CGo-7.3.77': ('cgo', 'cgo_binary', 'final'),
}


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def campaign_id(campaign, phase, workload):
    return hashlib.sha256(f'{campaign}/{phase}/{workload}'.encode()).hexdigest()[:29]


def pins(args):
    paths = [args.protocol, args.orders, args.harness_archive, *args.pin]
    for pattern in args.pin_glob:
        matched = sorted(glob.glob(pattern))
        if not matched:
            raise ValueError('pin glob matched nothing: ' + pattern)
        paths.extend(matched)
    resolved = []
    for p in paths:
        path = Path(p).resolve()
        if not path.is_file():
            raise ValueError('pinned path is not a regular file: ' + str(p))
        resolved.append(path)
    return sorted(set(resolved))


def make_args(args, phase, workload, pinned):
    arms = []
    for name in phase['arms']:
        plan_name, binary_option, revision_key = ARMS[name]
        arms.append(f'{plan_name}={getattr(args, binary_option)}@{args.revisions[revision_key]}')
    return argparse.Namespace(
        arm=arms, workload=[workload], iterations=phase['iterations'], warmup=phase['warmup'],
        blocks=phase['blocks'], cluster_file=args.cluster_file, native_library=args.native_library,
        server_binary=args.server_binary, harness_id=sha256(args.harness_archive), pin=[str(p) for p in pinned],
        campaign_id=campaign_id(args.campaign, phase['id'], workload), seed=phase['order_seed'],
        gomaxprocs=args.runtime['GOMAXPROCS'], housekeeping_cpus=args.runtime['housekeeping_cpus'],
        server_cpus=args.runtime['server_cpus'], client_cpus=args.runtime['client_cpus'],
        campaign_wall_cap_seconds=phase['per_workload_campaign_wall_cap_seconds'],
        timeout_seconds=phase['per_process_timeout_seconds'])


def check_plan(plan, phase, runtime):
    env = plan['environment']
    want_env = {'GOMAXPROCS': str(runtime['GOMAXPROCS']), 'GOGC': str(runtime['GOGC']),
                'GOMEMLIMIT': runtime['GOMEMLIMIT'], 'GODEBUG': runtime['GODEBUG'],
                'TZ': runtime['TZ'], 'LC_ALL': runtime['LC_ALL']}
    if env != want_env:
        raise ValueError(f'plan environment {env} differs from protocol runtime {want_env}')
    if plan['campaign_wall_cap_seconds'] != phase['per_workload_campaign_wall_cap_seconds']:
        raise ValueError('campaign wall cap differs from protocol')
    if plan['timeout_seconds'] != phase['per_process_timeout_seconds']:
        raise ValueError('per-process timeout differs from protocol')
    if plan['fdb_api_version'] != runtime['api'] or plan['go_version'] != 'go' + runtime['go']:
        raise ValueError('plan API/Go version differs from protocol runtime')


def check_schedule(schedule, phase, frozen):
    by_plan_name = {ARMS[name][0]: name for name in phase['arms']}
    blocks = sorted({r['block'] for r in schedule})
    if blocks != list(range(phase['blocks'])):
        raise ValueError(f'schedule blocks {blocks} differ from {phase["blocks"]}')
    for block in blocks:
        runs = sorted((r for r in schedule if r['block'] == block), key=lambda r: r['position'])
        order = [by_plan_name[r['arm']] for r in runs]
        if order != frozen[block]:
            raise ValueError(f'block {block} runs {order}, frozen order is {frozen[block]}')
    prefixes = [r['command'][r['command'].index('--prefix') + 1] for r in schedule]
    if len(set(prefixes)) != len(prefixes) or len({len(p.encode()) for p in prefixes}) != 1:
        raise ValueError('schedule prefixes are not unique and of equal length')
    return prefixes


# The default campaign command runs the harness's run.py directly. A campaign
# adapter (for example one that adds during-arm health checks around run.py)
# replaces it with --run-template, a JSON argv list whose strings may use
# {python}, {scripts}, {plan}, {output}, {phase}, {workload}, {netns},
# {housekeeping_cpus}, {outer_timeout} (the plan's campaign wall cap plus 20
# seconds, in seconds), {root} (FDB_BENCH_ROOT) and {server_addr}
# (FDB_BENCH_SERVER_ADDR, required only when used); the adapter file itself
# must be pinned with --pin.
DEFAULT_RUN_TEMPLATE = ['timeout', '--signal=TERM', '--kill-after=10s', '{outer_timeout}s', 'ip', 'netns', 'exec', '{netns}',
                        'taskset', '--cpu-list', '{housekeeping_cpus}', '{python}', '-B', '{scripts}/run.py',
                        '--plan', '{plan}', '--output', '{output}']


def run_command(args, phase, workload, plan, plan_path, output):
    values = dict(python=sys.executable, scripts=str(Path(args.scripts_dir).resolve()), plan=str(plan_path),
                  output=str(output), phase=phase, workload=workload, netns=args.netns,
                  housekeeping_cpus=args.runtime['housekeeping_cpus'],
                  outer_timeout=plan['campaign_wall_cap_seconds'] + 20, root=str(ROOT),
                  server_addr=os.environ.get('FDB_BENCH_SERVER_ADDR', ''))
    if any('{server_addr}' in part for part in args.run_template) and not values['server_addr']:
        raise ValueError('run template uses {server_addr}; set FDB_BENCH_SERVER_ADDR')
    command = [part.format(**values) for part in args.run_template]
    if str(plan_path) not in command or str(output) not in command:
        raise ValueError('run template must pass {plan} and {output}')
    return command


def seal(args):
    protocol = json.loads(Path(args.protocol).read_text())
    orders = json.loads(Path(args.orders).read_text())
    args.runtime = protocol['runtime']
    args.revisions = protocol['production_revisions']
    args.campaign = protocol['campaign']
    if protocol.get('harness_revision') != args.harness_revision:
        raise ValueError(f'protocol harness_revision {protocol.get("harness_revision")} != {args.harness_revision}')
    scripts = Path(args.scripts_dir).resolve()
    sys.path.insert(0, str(scripts))
    import matrix_plan  # noqa: E402  (the frozen harness's own generator)
    pinned = pins(args)
    out = Path(args.output_dir)
    out.mkdir(parents=True, exist_ok=False)
    manifest = dict(campaign=args.campaign, harness_revision=args.harness_revision, scripts_dir=str(scripts),
                    run_template=args.run_template,
                    generator=str(Path(__file__).resolve()), generator_sha256=sha256(__file__),
                    pinned_files={str(p): sha256(p) for p in pinned}, campaigns=[])
    for phase in protocol['phases']:
        if phase['id'] not in args.phase:
            continue
        frozen = orders[phase['frozen_order_key']]
        if frozen['seed'] != phase['order_seed'] or len(frozen['orders']) != phase['blocks']:
            raise ValueError(f'orders.json {phase["id"]} does not match the protocol phase')
        for workload in phase['workloads']:
            folder = out / phase['id'] / workload
            folder.mkdir(parents=True)
            plan = matrix_plan.make_plan(make_args(args, phase, workload, pinned))
            for w in plan['workloads']:
                w['workers'] = phase['workers']  # the only data override: closed-loop workers
            check_plan(plan, phase, args.runtime)
            plan_path = folder / 'plan.json'
            plan_path.write_text(json.dumps(plan, indent=2, sort_keys=True) + '\n')
            schedule_dir = folder / 'schedule'
            subprocess.run([sys.executable, '-B', str(scripts / 'run.py'), '--plan', str(plan_path),
                            '--output', str(schedule_dir), '--schedule-only'], check=True)
            schedule = json.loads((schedule_dir / 'schedule.json').read_text())
            prefixes = check_schedule(schedule, phase, frozen['orders'])
            if json.loads((schedule_dir / 'plan.json').read_text()) != plan:
                raise ValueError('run.py stored a different plan')
            manifest['campaigns'].append(dict(
                phase=phase['id'], workload=workload, campaign=plan['campaign'], seed=plan['seed'],
                blocks=plan['blocks'], arms=[a['name'] for a in plan['arms']],
                workers=phase['workers'], iterations=phase['iterations'], warmup=phase['warmup'],
                runs=len(schedule), prefix_bytes=len(prefixes[0].encode()),
                plan=str(plan_path), plan_sha256=sha256(plan_path),
                schedule=str(schedule_dir / 'schedule.json'), schedule_sha256=sha256(schedule_dir / 'schedule.json'),
                frozen_orders=frozen['orders'],
                campaign_wall_cap_seconds=plan['campaign_wall_cap_seconds'],
                timeout_seconds=plan['timeout_seconds'],
                run=run_command(args, phase['id'], workload, plan, plan_path,
                                Path(args.results_dir) / phase['id'] / workload)))
    (out / 'manifest.json').write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    return manifest


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('--scripts-dir', required=True, help='the frozen harness tools/clientbench/scripts on the VM')
    ap.add_argument('--protocol', required=True)
    ap.add_argument('--orders', required=True)
    ap.add_argument('--harness-archive', required=True, help='client harness archive; its SHA-256 is the harness ID')
    ap.add_argument('--harness-revision', required=True, help='must equal protocol.json harness_revision')
    ap.add_argument('--native-library', required=True)
    ap.add_argument('--server-binary', default='/usr/sbin/fdbserver')
    ap.add_argument('--cluster-file', default=str(ROOT/'cluster/fdb.cluster'))
    ap.add_argument('--before-binary', default=str(ROOT/'bin/client-before-go'))
    ap.add_argument('--final-binary', default=str(ROOT/'bin/client-final-go'))
    ap.add_argument('--cgo-binary', default=str(ROOT/'bin/client-final-cgo'))
    ap.add_argument('--pin', action='append', default=[], help='additional pinned file (receipts, configuration)')
    ap.add_argument('--pin-glob', action='append', default=[], help='pin every file a glob matches (sources, overlays)')
    ap.add_argument('--netns', default=NETNS)
    ap.add_argument('--phase', action='append', choices=PHASES, help='default: A and A2')
    ap.add_argument('--output-dir', required=True, help='new directory for plans, schedules and manifest.json')
    ap.add_argument('--results-dir', required=True, help='parent of each campaign output in the run commands')
    ap.add_argument('--run-template', type=json.loads, default=DEFAULT_RUN_TEMPLATE,
                    help='JSON argv list for each campaign command; see DEFAULT_RUN_TEMPLATE')
    args = ap.parse_args()
    args.phase = args.phase or list(PHASES)
    manifest = seal(args)
    print(f'{len(manifest["campaigns"])} sealed client campaigns; manifest {Path(args.output_dir) / "manifest.json"}')


if __name__ == '__main__':
    main()
