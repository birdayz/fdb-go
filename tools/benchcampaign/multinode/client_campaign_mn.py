#!/usr/bin/env python3
"""Multi-node M1 client adapter (R1) around the frozen run.py, derived from the
single-node client_campaign.py. It executes one (phase, workload) plan sealed by
`seal_multinode.py client-plans`; run.py owns data and measurement.

run --plan PLANS/<phase>/<workload>/plan.json --manifest-sha256 SHA
    --output RESULTS/<phase>/<workload> --phase M1wN --workload NAME
    --protocol FILE --orders FILE --layout cluster-ssd/layout.json --vpc-cidr CIDR
    --source-receipt FILE --lifetime-deadline ISO8601_UTC|EPOCH
    [--scripts DIR, default: the sealing manifest's scripts_dir]
The driver runs exactly the campaign's sealed `run` argv from
`seal_multinode.py client-plans --adapter ...`, rendered with
seal_multinode.render_run (manifest SHA-256 and lifetime deadline only).

Differences from the single-node adapter:
  * cluster guard: the generated three-coordinator cluster file (layout.json),
    every coordinator a distinct machine inside --vpc-cidr; no network
    namespace (the client reaches the cluster over the private interface);
  * engine, availability, recovery, durability lag and log queue come from
    `status json`; check-roles (fdb_cluster.py) runs before the campaign, before
    every arm and after the campaign, and roles/generation must not change;
  * data-distribution pre-arm gate: DD on and moving_data in-flight and queued
    bytes 0 before every arm, waiting up to 60 s and then aborting the
    campaign; after a wait the CPU quiescence gate is taken again;
  * the client machine must run no fdbserver;
  * lifetime gate: phase cap + 600 s collection + 300 s teardown verification;
  * every arm built from the final production revision (final-Go, CGo) is final
    tree: its correctness failure stops the phase.
One committed user-key clear before each workload campaign, never per arm.
The campaign directory keeps status-before/after.json and
check-roles-before/after.json (sha256 in case-receipts.json); run.py's
result.json carries each run's started_utc/ended_utc. Both feed
build_reconcile_input.py.
Pin (seal_multinode --pin) this file, cluster_gate.py, fdb_cluster.py,
mn_common.py, the single-node client_campaign.py and bench_runner.py, the
layout, the source receipt and its archives.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cluster_gate as cg  # noqa: E402
import client_campaign as cc  # noqa: E402  (single-node adapter: schedule/state/failure helpers)
import mn_common as mc  # noqa: E402

generic = cg.generic
EnvironmentFailure = cg.EnvironmentFailure
ARMS = cc.ARMS
REVISION_KEYS = {'before': 'before_allocation_work_after_read_parity', 'final': 'final', 'cgo': 'final'}
# The single-node forbidden set, plus fdbserver: no server process may run on the client.
FORBIDDEN = {'bazel', 'bazelisk', 'compile', 'cc1', 'cc1plus', 'ninja', 'make', 'gcc', 'g++', 'clang', 'clang++',
             'rustc', 'go', 'apt', 'apt-get', 'dpkg', 'perf', 'strace', 'fdbserver'} | set(generic.BINARIES)


def guarded_processes(owned):
    for pid, comm in cg.process_table():
        if pid not in owned and (comm in FORBIDDEN or comm.startswith(('client-', 'clientbench', 'baseline-', 'candidate-'))):
            raise EnvironmentFailure(f'concurrent build/profile/install/benchmark/server: {pid} {comm}')


def harness_revision(protocol, phase, workload):
    if workload in phase.get('new_harness_workloads', []):
        if not protocol.get('new_harness_revision'):
            raise ValueError(f'{workload} needs the reviewed new harness, but protocol new_harness_revision is unset')
        return protocol['new_harness_revision']
    return protocol['frozen_harness_revision']


def validate_plan(plan, schedule, protocol, orders, phase_id, workload):
    phase = next((p for p in protocol['phases'] if p['id'] == phase_id and p['kind'] == 'client'), None)
    if phase is None or workload not in phase['workloads']:
        raise ValueError('unknown client phase/workload')
    if plan['workloads'] != [dict(name=workload, workers=phase['workers'], iterations=phase['iterations'], warmup=phase['warmup'])]:
        raise ValueError('client workload/worker/sample configuration differs from protocol')
    if (plan['blocks'] != phase['blocks'] or plan['seed'] != phase['order_seed'] or
            plan['campaign_wall_cap_seconds'] != phase['per_workload_campaign_wall_cap_seconds'] or
            plan['timeout_seconds'] != phase['per_process_timeout_seconds']):
        raise ValueError('client cap/seed/blocks differ from protocol')
    arms = [ARMS[a] for a in phase['arms']]
    if [a['name'] for a in plan['arms']] != arms:
        raise ValueError('client arm population/order differs from protocol')
    runtime = protocol['runtime']
    if plan['environment'] != {k: str(runtime[k]) for k in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG', 'TZ', 'LC_ALL')}:
        raise ValueError('client runtime differs from protocol')
    if any(a['source_revision'] != protocol['production_revisions'][REVISION_KEYS[a['name']]] for a in plan['arms']):
        raise ValueError('client source identity differs from frozen protocol')
    policy = cg.client_vm_policy(protocol)
    if [(s['name'], s['cpus']) for s in plan['cpu_sets']] != [(s['name'], s['cpus']) for s in policy['cpu_sets']]:
        raise ValueError('client CPU groups differ from protocol runtime.client_vm')
    vm = runtime['client_vm']
    if any(s['command'][:3] != ['taskset', '--cpu-list', vm['client_cpus']] for s in schedule):
        raise ValueError('client affinity differs from protocol')
    frozen = orders[phase['frozen_order_key']]
    n = len(arms)
    if frozen['seed'] != phase['order_seed'] or len(schedule) != phase['blocks'] * n or len(frozen['orders']) != phase['blocks']:
        raise ValueError('empty/incomplete frozen client schedule')
    for b in range(phase['blocks']):
        runs = sorted([s for s in schedule if s['block'] == b], key=lambda s: s['position'])
        if [s['position'] for s in runs] != list(range(n)) or [s['arm'] for s in runs] != [ARMS[a] for a in frozen['orders'][b]]:
            raise ValueError('client order does not match frozen balanced block')
    prefixes = [s['command'][s['command'].index('--prefix') + 1] for s in schedule]
    if len(set(prefixes)) != len(prefixes) or len({len(p.encode()) for p in prefixes}) != 1:
        raise ValueError('client namespace population is not unique/equal length')
    return phase


def check_pair(before, after, plan):
    cg.telemetry_guard([before, after], plan)
    if cc.oom_count(after['/proc/vmstat']) != cc.oom_count(before['/proc/vmstat']):
        raise EnvironmentFailure('OOM during client process')


class Hooks(cc.Hooks):
    """The single-node hooks with the multi-node telemetry, process and server gates."""

    def __init__(self, helper, plan, schedule, out, gate):
        super().__init__(helper, plan, schedule, out)
        self.gate = gate

    def check_builds(self):
        guarded_processes(self.helper.descendants(self.pid) if self.pid else set())

    def telemetry(self, pid):
        self.pid = pid
        sample = self.original_telemetry(pid)
        sample['/proc/vmstat'] = Path('/proc/vmstat').read_text()
        # Retain the offending sample before raising (as single-node).
        with open(self.out / self.entry['run_id'] / 'adapter-telemetry.jsonl', 'a') as stream:
            stream.write(json.dumps(sample) + '\n')
        self.check_builds()
        if self.samples:
            check_pair(self.samples[-1], sample, self.plan)
        self.samples.append(sample)
        return sample

    def snapshot(self, dest):
        if dest.name == 'before':
            self.entry = self.entries[dest.parent.name]
            self.pid, self.samples = None, []
            if self.gate.pre_arm(dest.parent, 'adapter-status-before'):
                # run.py measured quiescence before this wait; measure it again.
                gate = self.helper.quiescence(self.plan)
                generic.dump(dest.parent / 'adapter-requiescence.json', gate)
                if not gate['passed']:
                    raise EnvironmentFailure('quiescence after waiting for data movement failed')
        self.original_snapshot(dest)
        if dest.name == 'after':
            # A terminal telemetry endpoint, including processes shorter than 1 s.
            self.telemetry(self.pid)
            before = (dest.parent / 'before/proc_vmstat').read_text()
            after = (dest / 'proc_vmstat').read_text()
            if cc.oom_count(before) != cc.oom_count(after):
                raise EnvironmentFailure('OOM in client lifecycle snapshots')
            self.gate.check(dest.parent, 'adapter-status-after')


def final_tree(protocol, entry, plan):
    revision = next(a['source_revision'] for a in plan['arms'] if a['name'] == entry['arm'])
    return revision == protocol['production_revisions']['final']


def run(args, command=None, helper=None, sleep=time.sleep, clock=time.monotonic):
    plan_path, out = Path(args.plan).resolve(), Path(args.output).resolve()
    mc.require_outside_repo(out, 'client campaign output')
    protocol_path, orders_path = Path(args.protocol).resolve(), Path(args.orders).resolve()
    layout_path, receipt_path = Path(args.layout).resolve(), Path(args.source_receipt).resolve()
    protocol, orders = json.loads(protocol_path.read_text()), json.loads(orders_path.read_text())
    layout, plan = json.loads(layout_path.read_text()), json.loads(plan_path.read_text())
    manifest_path = plan_path.parents[2] / 'manifest.json'
    if generic.sha(manifest_path) != args.manifest_sha256:
        raise ValueError('sealing manifest differs from the acknowledged SHA-256')
    manifest = json.loads(manifest_path.read_text())
    if manifest['protocol_sha256'] != generic.sha(protocol_path) or manifest['orders_sha256'] != generic.sha(orders_path):
        raise ValueError('protocol/orders differ from the sealing manifest')
    helper_path = (Path(args.scripts or manifest['scripts_dir']) / 'run.py').resolve()
    # Validate every code/config/source pin before importing the execute helper.
    required = cg.code_files(cc, sys.modules[__name__]) | {
        str(p) for p in (protocol_path, orders_path, helper_path, layout_path, receipt_path)}
    missing = sorted(required - set(plan['expected_sha256']))
    if missing:
        raise ValueError(f'adapter code, protocol, orders, run.py, layout and source receipt must be pinned: {missing}')
    before_pins = generic.verify_pins(plan)
    helper = helper or generic.load_helper(helper_path)
    schedule = helper.make_schedule(plan, out)
    phase = validate_plan(plan, schedule, protocol, orders, args.phase, args.workload)
    if phase['engine'] != layout['engine']:
        raise ValueError(f'phase engine {phase["engine"]} != layout engine {layout["engine"]}')
    sealed_path = plan_path.parent / 'schedule.json'
    if cc.normalize_schedule(schedule) != cc.normalize_schedule(json.loads(sealed_path.read_text())):
        raise ValueError('runtime client schedule differs from presealed schedule')
    receipt = next(c for c in manifest['campaigns'] if c['phase'] == args.phase and c['workload'] == args.workload)
    if (receipt.get('plan') != str(plan_path) or receipt.get('schedule') != str(sealed_path) or
            receipt['plan_sha256'] != generic.sha(plan_path) or receipt.get('schedule_sha256') != generic.sha(sealed_path)):
        raise ValueError('client plan/schedule path or SHA-256 differs from (or is missing in) the sealing manifest')
    cg.affinity_guard(generic.cpu_set(plan, 'housekeeping'))
    cluster = Path(schedule[0]['command'][schedule[0]['command'].index('--cluster-file') + 1])
    if str(cluster) not in plan['expected_sha256']:
        raise ValueError('cluster file is not pinned')
    harness = harness_revision(protocol, phase, args.workload)
    out.parent.mkdir(parents=True, exist_ok=True)
    # One client campaign at a time across phases; no benchmark before the
    # full phase budget and exact schedule are checked.
    with open(out.parent.parent / 'client-campaign.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        out.mkdir(exist_ok=False)
        args._created_out = True
        state_path = out.parent / 'client-phase-state.json'
        now = time.monotonic()
        left = cg.lifetime_left(args.lifetime_deadline)
        if state_path.exists():
            state = json.loads(state_path.read_text())
            if state['protocol_sha256'] != generic.sha(protocol_path) or state['phase'] != args.phase or state.get('final_failure'):
                raise EnvironmentFailure('phase inputs changed or final correctness failure already stopped phase')
            if args.workload in state['cases']:
                raise EnvironmentFailure('case already attempted; automatic retry forbidden')
        else:
            if left < phase['wall_cap_seconds'] + cg.LIFETIME_RESERVE_SECONDS:
                receipt = dict(reason='insufficient phase budget: cap + 600 s collection + 300 s teardown',
                               remaining=left, phase=args.phase, expected_cases=phase['workloads'])
                generic.dump(out / 'UNEXECUTED.json', receipt)
                generic.dump(out.parent / 'client-phase-unexecuted.json', receipt)
                return 2
            state = dict(phase=args.phase, phase_start_monotonic=now, protocol_sha256=generic.sha(protocol_path),
                         expected_cases=phase['workloads'], cases={})
        cc.workload_order_guard(state, phase, args.workload)
        state['cases'][args.workload] = dict(status='started')
        generic.dump(state_path, state)
        deadline = min(now + plan['campaign_wall_cap_seconds'], state['phase_start_monotonic'] + phase['wall_cap_seconds'],
                       now + left - cg.LIFETIME_RESERVE_SECONDS)
        generic.dump(out / 'plan.json', plan)
        generic.dump(out / 'schedule.json', schedule)
        generic.dump(out / 'adapter.json', dict(
            adapter_sha256=generic.sha(__file__), protocol_sha256=generic.sha(protocol_path),
            plan_sha256=generic.sha(plan_path), schedule_sha256=generic.sha(sealed_path),
            manifest_sha256=args.manifest_sha256, layout_sha256=generic.sha(layout_path), harness_revision=harness,
            lifetime_deadline=args.lifetime_deadline, campaign_deadline_monotonic=deadline,
            phase_wall_cap_seconds=phase['wall_cap_seconds'], expected_case_population=len(phase['workloads']),
            engine=phase['engine'], vpc_cidr=args.vpc_cidr, no_per_arm_cluster_clear=True))
        env = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'HOME': str(Path.home()),
               **plan['environment']}
        gate = cg.ServerGate(cluster, layout, phase['engine'], 'enabled', command or cg.default_command(helper, env),
                             sleep=sleep, clock=clock)
        hooks = Hooks(helper, plan, schedule, out, gate)
        hooks.install()
        measurement_started = False
        try:
            cg.cluster_file_guard(cluster, layout, args.vpc_cidr)
            generic.validate_source_receipt(json.loads(receipt_path.read_text()),
                                           dict(harness_commit=harness, production_revisions=protocol['production_revisions'],
                                                expected_sha256=plan['expected_sha256']))
            guarded_processes(set())
            try:
                gate.begin_case(out, 'adapter-campaign-before')
            finally:
                cg.persist_receipts(gate, out, 'before')
            gate.clear_user_keys(out, 'adapter-campaign-clear-user-keys')
            gate.check(out, 'adapter-campaign-after-clear')
            measurement_started = True
            rc = helper.execute(plan, schedule, str(plan_path), out, deadline)
            generic.verify_pins(plan)
            try:
                gate.check(out, 'adapter-campaign-after')
            finally:
                cg.persist_receipts(gate, out, 'after')
            state['cases'][args.workload] = dict(status='complete', runs=len(schedule))
            return rc
        except BaseException as exc:
            environmental = cc.environmental_failure(exc, hooks.entry, out, measurement_started)
            final = bool(hooks.entry and final_tree(protocol, hooks.entry, plan)) and not environmental
            status = 'environment-aborted' if environmental else ('final correctness-failed' if final else 'historical correctness-failed/not-comparable')
            adverse = dict(status=status, error=repr(exc), run=hooks.entry, contaminated_block=hooks.entry['block'] if hooks.entry else None,
                           no_ratios=True, no_retry=True, whole_case_aborted=True)
            generic.dump(out / 'ADAPTER_ADVERSE.json', adverse)
            state['cases'][args.workload] = adverse
            if final:
                state['final_failure'] = True
            raise
        finally:
            hooks.restore()
            generic.dump(state_path, state)
            generic.dump(out / 'adapter-pins.finally.json', {p: generic.sha(p) if Path(p).is_file() else None for p in before_pins})
            if set(state['cases']) == set(state['expected_cases']) and all(c['status'] == 'complete' for c in state['cases'].values()):
                generic.dump(out.parent / 'client-phase-complete.json', state)


def parser():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest='action', required=True)
    r = sub.add_parser('run')
    for name in ('plan', 'manifest-sha256', 'output', 'phase', 'workload', 'protocol', 'orders', 'layout', 'vpc-cidr',
                 'source-receipt', 'lifetime-deadline'):
        r.add_argument('--' + name, required=True)
    r.add_argument('--scripts', help="frozen tools/clientbench/scripts (default: the sealing manifest's scripts_dir)")
    return p


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        return run(args)
    except BaseException as exc:
        if getattr(args, '_created_out', False) and not (Path(args.output) / 'ADAPTER_ADVERSE.json').exists():
            generic.dump(Path(args.output) / 'ADAPTER_PREFLIGHT_ADVERSE.json',
                         dict(status='environment-aborted before execute', error=repr(exc), no_retry=True, no_ratios=True))
        raise


if __name__ == '__main__':
    sys.exit(main())
