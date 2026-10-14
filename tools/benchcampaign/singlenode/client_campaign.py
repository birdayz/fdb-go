#!/usr/bin/env python3
"""External A/A2 safety adapter; original pinned run.py owns data/measurement.

run --plan FILE --output NEWDIR --phase A|A2 --workload NAME
    --protocol FILE --orders FILE --lifetime-deadline ISO8601_UTC
    [--scripts $FDB_BENCH_ROOT/tools/clientbench/scripts]
    [--source-receipt $FDB_BENCH_ROOT/provenance/source-receipt.json]
self-test [--protocol FILE --orders FILE]: stdlib-only tests (default inputs:
    testdata/ beside this file); no FDB/process/SSH/benchmark execution.
run requires FDB_BENCH_SERVER_ADDR (owned server host:port); FDB_BENCH_ROOT and
FDB_BENCH_NETNS default to /srv/fdb-bench and fdb-bench-client.
Pin this file and bench_runner.py in every client plan. The deadline is the hard
lifetime deadline; first phase invocation reserves its full cap plus 600 seconds.
One committed user-key clear before each workload campaign, never per arm;
fresh unique equal-length arm prefixes remain as sealed. SSD engine is mandatory.
"""
import argparse
import copy
import datetime as dt
import fcntl
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import unittest
from unittest import mock

import bench_runner as generic

ARMS = {'before-Go': 'before', 'final-Go': 'final', 'CGo-7.3.77': 'cgo'}
HERE = Path(__file__).resolve().parent
TEST_INPUTS = dict(protocol=HERE/'testdata/protocol.json', orders=HERE/'testdata/orders.json')


def normalize_schedule(schedule):
    value = copy.deepcopy(schedule)
    for entry in value:
        command = entry['command']
        if command.count('--output') != 1:
            raise ValueError('exactly one client output argument required')
        command[command.index('--output')+1] = '<run-output>'
    return value


def validate_plan(plan, schedule, protocol, orders, phase_id, workload):
    phase = next(p for p in protocol['phases'] if p['id'] == phase_id)
    if protocol['version'] not in (3, 4) or phase_id not in ('A', 'A2') or workload not in phase['workloads']:
        raise ValueError('unknown client phase/workload')
    if plan['workloads'] != [dict(name=workload, workers=phase['workers'], iterations=phase['iterations'], warmup=phase['warmup'])]:
        raise ValueError('client workload/worker/sample configuration differs from protocol')
    if (plan['blocks'] != phase['blocks'] or plan['seed'] != phase['order_seed'] or
            plan['campaign_wall_cap_seconds'] != phase['per_workload_campaign_wall_cap_seconds'] or
            plan['timeout_seconds'] != phase['per_process_timeout_seconds']):
        raise ValueError('client cap/seed/blocks differ from protocol')
    if [a['name'] for a in plan['arms']] != [ARMS[a] for a in phase['arms']]:
        raise ValueError('client arm population/order differs from protocol')
    runtime = protocol['runtime']
    if plan['environment'] != {k: str(runtime[k]) for k in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG', 'TZ', 'LC_ALL')}:
        raise ValueError('client runtime differs from protocol')
    expected_sources = {'before': protocol['production_revisions']['before_allocation_work_after_read_parity'],
                        'final': protocol['production_revisions']['final'], 'cgo': protocol['production_revisions']['final']}
    if any(a['source_revision'] != expected_sources[a['name']] for a in plan['arms']):
        raise ValueError('client source identity differs from frozen protocol')
    if any(s['command'][:3] != ['taskset', '--cpu-list', runtime['client_cpus']] for s in schedule):
        raise ValueError('client affinity differs from protocol')
    frozen = orders[phase['frozen_order_key']]
    if frozen['seed'] != phase['order_seed'] or len(schedule) != phase['blocks']*3 or len(frozen['orders']) != phase['blocks']:
        raise ValueError('empty/incomplete frozen client schedule')
    for b in range(phase['blocks']):
        runs = sorted([s for s in schedule if s['block'] == b], key=lambda s: s['position'])
        if [s['position'] for s in runs] != [0, 1, 2] or [s['arm'] for s in runs] != [ARMS[a] for a in frozen['orders'][b]]:
            raise ValueError('client order does not match frozen balanced block')
    prefixes = [s['command'][s['command'].index('--prefix')+1] for s in schedule]
    if len(set(prefixes)) != len(prefixes) or len({len(p.encode()) for p in prefixes}) != 1:
        raise ValueError('client namespace population is not unique/equal length')
    return phase


def cluster_scope_guard(cluster):
    if str(cluster) != generic.CLUSTER or not re.fullmatch(r'[^\s@]+@'+re.escape(generic.server_addr()), cluster.read_text().strip()):
        raise generic.EnvironmentFailure('client is not using the owned private cluster')
    if Path(generic.ENGINE).read_text().strip() != 'ssd':
        raise generic.EnvironmentFailure('client campaigns require owned SSD engine')


def workload_order_guard(state, phase, workload):
    index = phase['workloads'].index(workload)
    if set(state['cases']) != set(phase['workloads'][:index]):
        raise generic.EnvironmentFailure('previously attempted cases must be the exact frozen workload prefix')


def clear_campaign(folder, helper, env):
    generic.health(folder, 'adapter-campaign-before-clear', helper, env)
    text = generic.controlled_command(['fdbcli', '-C', generic.CLUSTER, '--exec',
                                       'writemode on; clearrange "" "\\xff"'],
                                      folder, 'adapter-campaign-clear-user-keys', helper, env)
    if 'Committed' not in text:
        raise generic.EnvironmentFailure('missing committed campaign user-key clear receipt')
    generic.health(folder, 'adapter-campaign-after-clear', helper, env)


def oom_count(text):
    match = re.search(r'^oom_kill (\d+)$', text, re.M)
    if not match:
        raise generic.EnvironmentFailure('missing OOM telemetry counter')
    return int(match.group(1))


def check_pair(before, after, plan):
    generic.telemetry_guard([before, after], plan)
    if oom_count(after['/proc/vmstat']) != oom_count(before['/proc/vmstat']):
        raise generic.EnvironmentFailure('OOM during client process')


def environmental_failure(exc, entry, out, measurement_started):
    if not measurement_started or isinstance(exc, (generic.EnvironmentFailure, OSError, TimeoutError, KeyboardInterrupt)) or 'quiescence' in str(exc) or 'changed during campaign' in str(exc):
        return True
    if entry:
        folder = out/entry['run_id']
        result = folder/'result.json'
        if result.exists():
            rc = json.loads(result.read_text()).get('returncode', 0)
            if isinstance(rc, int) and (rc < 0 or rc >= 128):
                return True
            stderr = folder/'stderr.txt'
            if rc and stderr.exists() and re.search(r'signal|timeout|timed out|killed', stderr.read_text(), re.I):
                return True
    return False


def guarded_processes(owned):
    found = subprocess.run(['ps', '-eo', 'pid=,comm='], check=True, capture_output=True, text=True)
    forbidden = {'bazel', 'bazelisk', 'compile', 'cc1', 'cc1plus', 'ninja', 'make', 'gcc', 'g++',
                 'clang', 'clang++', 'rustc', 'go', 'apt', 'apt-get', 'dpkg', 'perf', 'strace'} | set(generic.BINARIES)
    for line in found.stdout.splitlines():
        pid, comm = line.split(maxsplit=1)
        if int(pid) not in owned and (comm in forbidden or comm.startswith(('client-', 'clientbench', 'baseline-', 'candidate-'))):
            raise generic.EnvironmentFailure('concurrent build/profile/install/benchmark: '+line)


class Hooks:
    def __init__(self, helper, plan, schedule, out):
        self.helper, self.plan, self.out = helper, plan, out
        self.entries = {r['run_id']: r for r in schedule}
        self.entry = None
        self.pid = None
        self.samples = []
        self.original_telemetry = helper.telemetry
        self.original_snapshot = helper.snapshot
        self.original_validate = helper.validate_raw_output
        self.original_check = helper.check_builds
        self.env = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
                    'HOME': str(Path.home()), **plan['environment']}

    def check_builds(self):
        guarded_processes(self.helper.descendants(self.pid) if self.pid else set())

    def telemetry(self, pid):
        self.pid = pid
        sample = self.original_telemetry(pid)
        sample['/proc/vmstat'] = Path('/proc/vmstat').read_text()
        # Retain the offending sample before raising; the original execute loop
        # cannot write a callback's return value if the callback raises.
        folder = self.out/self.entry['run_id']
        with open(folder/'adapter-telemetry.jsonl', 'a') as stream:
            stream.write(json.dumps(sample)+'\n')
        self.check_builds()
        if self.samples:
            check_pair(self.samples[-1], sample, self.plan)
        self.samples.append(sample)
        return sample

    def snapshot(self, dest):
        if dest.name == 'before':
            self.entry = self.entries[dest.parent.name]
            self.pid, self.samples = None, []
            generic.health(dest.parent, 'adapter-status-before', self.helper, self.env)
        self.original_snapshot(dest)
        if dest.name == 'after':
            # Always add a terminal telemetry endpoint, including processes
            # shorter than 1 second; no complete interval means no steal claim.
            self.telemetry(self.pid)
            before = (dest.parent/'before/proc_vmstat').read_text()
            after = (dest/'proc_vmstat').read_text()
            if oom_count(before) != oom_count(after):
                raise generic.EnvironmentFailure('OOM in client lifecycle snapshots')
            generic.health(dest.parent, 'adapter-status-after', self.helper, self.env)

    def validate(self, folder, workload):
        self.original_validate(folder, workload)
        if len(self.samples) < 2:
            raise generic.EnvironmentFailure('missing client telemetry endpoints')
        self.pid = None
        generic.dump(folder/'adapter-guard.json', dict(passed=True, samples=len(self.samples),
                     complete_intervals=sum((b['monotonic_ns']-a['monotonic_ns'])/1e9 >= .95 for a, b in zip(self.samples, self.samples[1:])),
                     block=self.entry['block'], no_per_arm_cluster_clear=True))

    def install(self):
        self.helper.telemetry, self.helper.snapshot = self.telemetry, self.snapshot
        self.helper.validate_raw_output, self.helper.check_builds = self.validate, self.check_builds

    def restore(self):
        self.helper.telemetry, self.helper.snapshot = self.original_telemetry, self.original_snapshot
        self.helper.validate_raw_output, self.helper.check_builds = self.original_validate, self.original_check


def run(args):
    plan_path, out = Path(args.plan).resolve(), Path(args.output).resolve()
    protocol_path, orders_path = Path(args.protocol).resolve(), Path(args.orders).resolve()
    protocol, orders = json.loads(protocol_path.read_text()), json.loads(orders_path.read_text())
    plan = json.loads(plan_path.read_text())
    helper_path = Path(args.scripts).resolve()/'run.py'
    # Validate every code/config/source pin before importing the execute helper.
    required = {str(Path(__file__).resolve()), str(Path(generic.__file__).resolve()), str(protocol_path), str(orders_path), str(helper_path), generic.ENGINE, str(Path(args.source_receipt).resolve())}
    if not required.issubset(plan['expected_sha256']):
        raise ValueError('adapter/generic helper/protocol/orders/run.py must all be pinned')
    before_pins = generic.verify_pins(plan)
    helper = generic.load_helper(helper_path)
    schedule = helper.make_schedule(plan, out)
    phase = validate_plan(plan, schedule, protocol, orders, args.phase, args.workload)
    sealed_path = plan_path.parent/'schedule/schedule.json'
    sealed = json.loads(sealed_path.read_text())
    if normalize_schedule(schedule) != normalize_schedule(sealed):
        raise ValueError('runtime client schedule differs from presealed schedule')
    manifest = json.loads((plan_path.parents[2]/'manifest.json').read_text())
    receipt = next(c for c in manifest['campaigns'] if c['phase'] == args.phase and c['workload'] == args.workload)
    if receipt['plan_sha256'] != generic.sha(plan_path) or receipt['schedule_sha256'] != generic.sha(sealed_path):
        raise ValueError('client plan/schedule differs from sealing manifest')
    if (sorted(os.sched_getaffinity(0)) != generic.cpu_set(plan, 'housekeeping') or
            os.stat('/proc/self/ns/net').st_ino != os.stat('/var/run/netns/'+generic.NETNS).st_ino):
        raise generic.EnvironmentFailure('client adapter must run in private namespace on the housekeeping CPUs')
    cluster = Path(schedule[0]['command'][schedule[0]['command'].index('--cluster-file')+1])
    out.parent.mkdir(parents=True, exist_ok=True)
    # One client campaign at a time, even across A and A2. No live benchmark
    # is admitted before the full phase budget and exact schedule are checked.
    with open(out.parent.parent/'client-campaign.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        out.mkdir(exist_ok=False)
        args._created_out = True
        state_path = out.parent/'client-phase-state.json'
        now = time.monotonic()
        hard = dt.datetime.fromisoformat(args.lifetime_deadline.replace('Z', '+00:00'))
        left = (hard-dt.datetime.now(dt.timezone.utc)).total_seconds()
        if state_path.exists():
            state = json.loads(state_path.read_text())
            if state['protocol_sha256'] != generic.sha(protocol_path) or state['phase'] != args.phase or state.get('final_failure'):
                raise generic.EnvironmentFailure('phase inputs changed or final correctness failure already stopped phase')
            if args.workload in state['cases']:
                raise generic.EnvironmentFailure('case already attempted; automatic retry forbidden')
        else:
            if left < phase['wall_cap_seconds']+600:
                receipt = dict(reason='insufficient full phase budget plus600s collection', remaining=left, phase=args.phase, expected_cases=phase['workloads'])
                generic.dump(out/'UNEXECUTED.json', receipt)
                generic.dump(out.parent/'client-phase-unexecuted.json', receipt)
                return 2
            state = dict(phase=args.phase, phase_start_monotonic=now, protocol_sha256=generic.sha(protocol_path),
                         expected_cases=phase['workloads'], cases={})
        workload_order_guard(state, phase, args.workload)
        state['cases'][args.workload] = dict(status='started')
        generic.dump(state_path, state)
        deadline = min(now+plan['campaign_wall_cap_seconds'], state['phase_start_monotonic']+phase['wall_cap_seconds'], now+left-600)
        generic.dump(out/'plan.json', plan)
        generic.dump(out/'schedule.json', schedule)
        generic.dump(out/'adapter.json', dict(adapter_sha256=generic.sha(__file__), protocol_sha256=generic.sha(protocol_path),
                     plan_sha256=generic.sha(plan_path), schedule_sha256=generic.sha(sealed_path),
                     harness_revision=protocol['harness_revision'], lifetime_deadline=args.lifetime_deadline,
                     campaign_deadline_monotonic=deadline, phase_wall_cap_seconds=phase['wall_cap_seconds'],
                     expected_case_population=len(phase['workloads']), no_per_arm_cluster_clear=True))
        hooks = Hooks(helper, plan, schedule, out)
        hooks.install()
        measurement_started = False
        try:
            cluster_scope_guard(cluster)
            generic.validate_source_receipt(json.loads(Path(args.source_receipt).read_text()),
                                           dict(harness_commit=protocol['harness_revision'], production_revisions=protocol['production_revisions'], expected_sha256=plan['expected_sha256']))
            guarded_processes(set())
            clear_campaign(out, helper, hooks.env)
            measurement_started = True
            rc = helper.execute(plan, schedule, str(plan_path), out, deadline)
            generic.verify_pins(plan)
            state['cases'][args.workload] = dict(status='complete', runs=len(schedule))
            return rc
        except BaseException as exc:
            environmental = environmental_failure(exc, hooks.entry, out, measurement_started)
            final = bool(hooks.entry and hooks.entry['arm'] == 'final') and not environmental
            status = 'environment-aborted' if environmental else ('final correctness-failed' if final else 'historical correctness-failed/not-comparable')
            adverse = dict(status=status, error=repr(exc), run=hooks.entry, contaminated_block=hooks.entry['block'] if hooks.entry else None,
                           no_ratios=True, no_retry=True, whole_case_aborted=True)
            generic.dump(out/'ADAPTER_ADVERSE.json', adverse)
            state['cases'][args.workload] = adverse
            if final:
                state['final_failure'] = True
            raise
        finally:
            hooks.restore()
            generic.dump(state_path, state)
            generic.dump(out/'adapter-pins.finally.json', {p: generic.sha(p) if Path(p).is_file() else None for p in before_pins})
            if set(state['cases']) == set(state['expected_cases']) and all(c['status'] == 'complete' for c in state['cases'].values()):
                generic.dump(out.parent/'client-phase-complete.json', state)


class AdapterTests(unittest.TestCase):
    def test_complete_short_interval_and_steal(self):
        def sample(n, steal=0):
            return dict(monotonic_ns=int(n*1e9), processes=[dict(pid=12, comm='fdbserver')],
                        **{'/proc/vmstat': 'oom_kill 0\n', '/proc/stat': '\n'.join('cpu'+str(i)+f' {int(n*10)} 0 0 {int(n*90)} 0 0 0 {steal}' for i in range(16))})
        plan = {'max_steal_fraction': .01, 'cpu_sets': [dict(name='housekeeping', cpus=[0, 1]),
                                                       dict(name='server', cpus=list(range(2, 8))),
                                                       dict(name='client', cpus=list(range(8, 16)))]}
        check_pair(sample(1), sample(1.1), plan)
        check_pair(sample(1), sample(2), plan)
        with self.assertRaises(generic.EnvironmentFailure):
            check_pair(sample(1), sample(2, 2), plan)
        b = sample(2)
        b['/proc/vmstat'] = 'oom_kill 1\n'
        with self.assertRaises(generic.EnvironmentFailure):
            check_pair(sample(1), b, plan)

    def test_schedule_only_output_path_is_normalized(self):
        a = [dict(command=['client', '--output', '/seal/run', '--prefix', 'fixed'])]
        b = [dict(command=['client', '--output', '/results/run', '--prefix', 'fixed'])]
        self.assertEqual(normalize_schedule(a), normalize_schedule(b))
        b[0]['command'][-1] = 'changed'
        self.assertNotEqual(normalize_schedule(a), normalize_schedule(b))

    def test_forbidden_processes_exclude_owned_only(self):
        with mock.patch.object(subprocess, 'run', return_value=argparse.Namespace(stdout='10 client-final-go\n11 compile\n')):
            with self.assertRaises(generic.EnvironmentFailure):
                guarded_processes({10})
            guarded_processes({10, 11})

    def test_exact_protocol_cap_workers_and_orders(self):
        protocol = json.loads(Path(TEST_INPUTS['protocol']).read_text())
        orders = json.loads(Path(TEST_INPUTS['orders']).read_text())
        for pid in ('A', 'A2'):
            phase = next(p for p in protocol['phases'] if p['id'] == pid)
            for workload in phase['workloads']:
                sources = {'before': protocol['production_revisions']['before_allocation_work_after_read_parity'], 'final': protocol['production_revisions']['final'], 'cgo': protocol['production_revisions']['final']}
                plan = dict(blocks=phase['blocks'], seed=phase['order_seed'], campaign_wall_cap_seconds=phase['per_workload_campaign_wall_cap_seconds'],
                            timeout_seconds=phase['per_process_timeout_seconds'], workloads=[dict(name=workload, workers=phase['workers'], iterations=phase['iterations'], warmup=phase['warmup'])],
                            arms=[dict(name=ARMS[a], source_revision=sources[ARMS[a]]) for a in phase['arms']],
                            environment={k: str(protocol['runtime'][k]) for k in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG', 'TZ', 'LC_ALL')})
                schedule = [dict(block=b, position=i, arm=ARMS[a], command=['taskset', '--cpu-list', protocol['runtime']['client_cpus'], '--prefix', f'case/b{b}/p{i}'])
                            for b, order in enumerate(orders[pid]['orders']) for i, a in enumerate(order)]
                validate_plan(plan, schedule, protocol, orders, pid, workload)
                wrong = copy.deepcopy(schedule)
                wrong[0]['arm'] = 'unknown'
                with self.assertRaises(ValueError):
                    validate_plan(plan, wrong, protocol, orders, pid, workload)
                wrong_plan = copy.deepcopy(plan)
                wrong_plan['campaign_wall_cap_seconds'] += 1
                with self.assertRaises(ValueError):
                    validate_plan(wrong_plan, schedule, protocol, orders, pid, workload)

    def test_clear_argv_is_user_only_and_once_per_campaign(self):
        with mock.patch.object(generic, 'health') as health, mock.patch.object(generic, 'controlled_command', return_value='Committed (version1)') as command:
            clear_campaign(Path('/fake-new-case'), object(), {})
            self.assertEqual(command.call_count, 1)
            argv = command.call_args.args[0]
            self.assertEqual(argv[:4], ['fdbcli', '-C', generic.CLUSTER, '--exec'])
            self.assertEqual(argv[4], r'writemode on; clearrange "" "\xff"')
            self.assertEqual(health.call_count, 2)

    def test_engine_and_cluster_scope_refusals(self):
        with mock.patch.dict(os.environ, FDB_BENCH_SERVER_ADDR='192.0.2.1:4500'):
            with mock.patch.object(Path, 'read_text', side_effect=['private:key@192.0.2.1:4500', 'ssd']):
                cluster_scope_guard(Path(generic.CLUSTER))
            for texts in [['private:key@198.51.100.7:4500'], ['private:key@192.0.2.1:4500', 'memory'],
                          ['private:key@192.0.2.1:45000']]:
                with mock.patch.object(Path, 'read_text', side_effect=texts), self.assertRaises(generic.EnvironmentFailure):
                    cluster_scope_guard(Path(generic.CLUSTER))
        with mock.patch.dict(os.environ, {}, clear=True), self.assertRaisesRegex(generic.EnvironmentFailure, 'FDB_BENCH_SERVER_ADDR'):
            cluster_scope_guard(Path(generic.CLUSTER))

    def test_frozen_workload_prefix_prevents_reorder(self):
        phase = dict(workloads=['grv', 'get100', 'range10'])
        workload_order_guard(dict(cases={}), phase, 'grv')
        workload_order_guard(dict(cases={'grv': {}}), phase, 'get100')
        for state, workload in [(dict(cases={}), 'get100'), (dict(cases={'get100': {}}), 'range10'), (dict(cases={'grv': {}}), 'grv')]:
            with self.assertRaises(generic.EnvironmentFailure):
                workload_order_guard(state, phase, workload)

    def test_environmental_process_failure_classification(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            out = Path(directory)
            entry = dict(run_id='run')
            folder = out/'run'
            folder.mkdir()
            for rc, stderr, expected in [(-9, '', True), (137, '', True), (1, 'process timeout', True), (1, 'bad value', False), (0, '', False)]:
                generic.dump(folder/'result.json', dict(returncode=rc))
                (folder/'stderr.txt').write_text(stderr)
                self.assertEqual(environmental_failure(RuntimeError('workload failure'), entry, out, True), expected)
            self.assertTrue(environmental_failure(TimeoutError(), None, out, True))
            self.assertTrue(environmental_failure(ValueError('source receipt'), None, out, False))

    def test_missing_oom_counter_rejected(self):
        with self.assertRaises(generic.EnvironmentFailure):
            oom_count('no counter')


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest='action', required=True)
    test_parser = sub.add_parser('self-test')
    test_parser.add_argument('--protocol', default=str(TEST_INPUTS['protocol']))
    test_parser.add_argument('--orders', default=str(TEST_INPUTS['orders']))
    run_parser = sub.add_parser('run')
    for name in ('plan', 'output', 'phase', 'workload', 'protocol', 'orders', 'lifetime-deadline'):
        run_parser.add_argument('--'+name, required=True)
    run_parser.add_argument('--scripts', default=generic.HELPER.rsplit('/', 1)[0])
    run_parser.add_argument('--source-receipt', default=str(generic.ROOT/'provenance/source-receipt.json'))
    args = p.parse_args()
    if args.action == 'self-test':
        TEST_INPUTS.update(protocol=args.protocol, orders=args.orders)
        suite = unittest.defaultTestLoader.loadTestsFromTestCase(AdapterTests)
        return 0 if unittest.TextTestRunner(verbosity=2).run(suite).wasSuccessful() else 1
    try:
        return run(args)
    except BaseException as exc:
        if getattr(args, '_created_out', False) and not (Path(args.output)/'ADAPTER_ADVERSE.json').exists():
            generic.dump(Path(args.output)/'ADAPTER_PREFLIGHT_ADVERSE.json', dict(status='environment-aborted before execute', error=repr(exc), no_retry=True, no_ratios=True))
        raise


if __name__ == '__main__':
    sys.exit(main())
