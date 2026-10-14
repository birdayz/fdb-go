#!/usr/bin/env python3
"""Multi-node testing.B runner (R2), derived from the single-node bench_runner.py.
It executes one phase plan sealed by `seal_multinode.py testingb-plans`. Never
builds, never retries.

run --manifest PLANS/manifest.json --manifest-sha256 SHA --phase M2rl
    --protocol FILE --orders FILE --layout cluster-ssd/layout.json --vpc-cidr CIDR
    --scripts DIR --source-receipt FILE --placement-command JSON
    --output NEW_DIRECTORY --lifetime-deadline ISO8601_UTC|EPOCH

--placement-command is a JSON argv list run before the first and after the last
process of every case. It must write a fresh placement_receipt.py receipt for
the case's store to {output}; other placeholders: {store} {case} {when}
(before|after) {preset} {layout} {record_format} {cluster_file}. Every absolute
file path in it must be pinned in the sealing manifest.

Per phase: the manifest must hash to --manifest-sha256 (the acknowledged seal),
the plan to its manifest entry, every manifest pin must still hold, and the plan
is rebuilt with seal_multinode.testingb_plan from protocol, orders and the
sealed placement receipts and must equal the sealed plan.
Per case (protocol placement.stability and environment_policy):
  * the sealed placement receipt still hashes to the sha256 sealed in the plan;
  * status json server gate (check-roles, engine, DD frozen, no movement,
    durability lag/log queue) whose roles and generation become the case baseline;
  * a fresh `before` receipt with integrity, for the case's store/preset/cell,
    equal (placement_receipt.compare) to the sealed receipt; otherwise the case
    is omitted before any process runs;
  * after the last process: a fresh `after` receipt equal to `before`, and an
    unchanged status/roles/generation; otherwise the case is contaminated and
    omitted (its raw data retained).
Every job's result.json records run_id, started_utc and ended_utc (run.py's UTC
format) around its process; each case directory keeps status-before/after.json,
check-roles-before/after.json (sha256 in case-receipts.json) and the
compare-sealed-before/compare-before-after.json placement compares, for
build_reconcile_input.py.
Per arm: as single-node, but no user-key clear (the benchmark reseeds its own
fixture in its frozen shards; a whole-keyspace clear would remove the other
stores' placed fixtures), the server pre-arm gate instead of an engine file,
and the client machine must run no fdbserver.
Pin (seal_multinode --pin) this file, cluster_gate.py, fdb_cluster.py,
mn_common.py, seal_multinode.py, placement_receipt.py, the single-node
bench_runner.py and client_campaign.py, run.py, the layout, the source receipt
and its archives.
"""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cluster_gate as cg  # noqa: E402
import client_campaign as cc  # noqa: E402  (pinned with the gate, which needs it beside bench_runner)
import mn_common as mc  # noqa: E402
import placement_receipt as pr  # noqa: E402
import seal_multinode as sm  # noqa: E402

generic = cg.generic
EnvironmentFailure, CorrectnessFailure = cg.EnvironmentFailure, cg.CorrectnessFailure
FETCH_ROWS = {'limit1': 1, 'rows10': 10, 'rows100': 100, 'rows1000': 1000}
SKIP_ROWS = {'rows10': 10, 'rows100': 100, 'rows1000': 997}  # Skip 3 over [100,113), [100,203), all 1000
SQL_ROWS = {'rows10': 10, 'rows100': 100, 'rows1000': 1000}
PLACEMENT_TIMEOUT_SECONDS = 600


def audit_case(case):
    """The single-node case shape parse_output/validate_audits check a sealed multi-node case against."""
    stem, cell = case['stem'], case['cell']
    if stem.startswith('BenchmarkIndexFetch/'):
        small = case['payload'] == 'small'
        return dict(key=case['key'], stem=stem, binary_kind='fetch', audit='fetch', rows=FETCH_ROWS[cell], write=False,
                    fields=dict(payload=case['payload'], chunks=1 if small else 2, payload_bytes=0 if small else 100000,
                                reverse=case['dir'] == 'reverse', skip=0, limit=1 if cell == 'limit1' else 0))
    if stem.startswith('BenchmarkPlanSkip/'):
        return dict(key=case['key'], stem=stem, binary_kind='planskip', audit='planskip', rows=SKIP_ROWS[cell], write=False,
                    fields=dict(payload='small', reverse=case['dir'] == 'reverse', skip=3, limit=0))
    if stem.startswith('BenchmarkSQLIndexFetch/'):
        return dict(key=case['key'], stem=stem, binary_kind='sqlfetch', audit='sql-fetch', rows=SQL_ROWS[cell],
                    write=False, fields={})
    raise ValueError('unknown testing.B case ' + stem)


def rebuild(plan, protocol, orders, phase_id):
    """seal_multinode.testingb_plan from the pinned inputs; must equal the sealed plan."""
    if orders != sm.make_orders(protocol):
        raise EnvironmentFailure('orders.json differs from the protocol seeds')
    phase = next((p for p in protocol['phases'] if p['id'] == phase_id and p['kind'] == 'testingb'), None)
    if phase is None or plan.get('phase') != phase_id:
        raise EnvironmentFailure('unknown testing.B phase or plan for another phase')
    binaries = {j['command'][3] for j in plan['jobs']}
    cluster = plan['env']['FDB_BENCH_CLUSTER_FILE']
    if len(binaries) != 1 or set(plan['expected_sha256']) != binaries | {cluster}:
        raise EnvironmentFailure('plan must pin exactly its one binary and the cluster file')
    receipts = {c['store']: c['placement_receipt'] for c in plan['cases'] if 'placement_receipt' in c}
    allowed = {c['key'] for c in plan['cases']}
    rebuilt, _ = sm.testingb_plan(protocol, phase, orders[phase['frozen_order_key']]['orders'], next(iter(binaries)),
                                  cluster, allowed, receipts)
    rebuilt['expected_sha256'] = plan['expected_sha256']
    if rebuilt != plan:
        raise EnvironmentFailure('plan differs from the one sealed from protocol, orders and placement receipts')
    keys = [j['case'] for j in plan['jobs']]
    if [c['key'] for c in plan['cases']] != list(dict.fromkeys(keys)):
        raise EnvironmentFailure('jobs are not case-major in sealed case order')
    return phase


def placement_argv(template, case, when, output, cluster):
    values = dict(store=case['store'], case=case['key'], when=when, output=str(output), preset=case['placement_preset'],
                  layout=case['layout'], record_format=case['record_format'] or '', cluster_file=cluster)
    argv = [part.format(**values) for part in template]
    if str(output) not in argv:
        raise ValueError('placement command must pass {output}')
    return argv


def check_fresh_receipt(receipt, case, sealed, compare_path):
    """A fresh receipt must have integrity, be for this case's store and cell, and equal the reference.

    The compare result is written to compare_path first, whatever it says: R4
    reads it as the case's placement-stability receipt."""
    diff = pr.compare(sealed, receipt)
    generic.dump(compare_path, diff, exclusive=True)
    if receipt.get('format') != pr.RECEIPT_FORMAT or receipt.get('integrity_ok') is not True or receipt.get('error_count') != 0:
        raise EnvironmentFailure(f'placement receipt for {case["key"]} lacks integrity: {receipt.get("errors", [])[:3]}')
    for field, want in (('store', case['store']), ('layout', case['layout']), ('record_format', case['record_format']),
                        ('preset', case['placement_preset'])):
        if receipt.get(field) != want:
            raise EnvironmentFailure(f'placement receipt {field} {receipt.get(field)!r} != case {want!r}')
    lo, hi = pr.PRESETS[case['placement_preset']][case['placement_cell']]
    if ((receipt.get('cells') or {}).get(case['placement_cell']) or {}).get('ids') != [lo, hi]:
        raise EnvironmentFailure(f'placement receipt lacks cell {case["placement_cell"]} [{lo}, {hi})')
    if not diff['equal']:
        raise EnvironmentFailure('placement changed: ' + '; '.join(diff['differences']))
    return diff


def ensure_idle(helper, binary, owned=()):
    """Single-node ensure_idle plus fdbserver: no server process may run on the client."""
    helper.check_builds()
    forbidden = set(generic.BINARIES) | {Path(binary).name, 'fdbserver'}
    for pid, name in cg.process_table():
        if name in forbidden and pid not in owned:
            raise EnvironmentFailure(f'another benchmark or server process is active: {pid} {name}')


class Case:
    """Case-level placement and server checks around a contiguous block of jobs."""

    def __init__(self, case, folder, gate, command, template, cluster, pinned):
        self.case, self.folder, self.gate, self.command = case, folder, gate, command
        self.template, self.cluster, self.pinned = template, cluster, pinned
        self.before = None

    def placement(self, when):
        output = self.folder / f'placement-{when}.json'
        self.command(placement_argv(self.template, self.case, when, output, self.cluster), self.folder,
                     f'placement-{when}', PLACEMENT_TIMEOUT_SECONDS)
        try:
            receipt = json.loads(output.read_text())
        except (OSError, ValueError) as exc:
            raise EnvironmentFailure(f'placement command wrote no readable receipt: {exc}') from exc
        if not isinstance(receipt, dict):
            raise EnvironmentFailure('placement receipt is not a JSON object')
        return receipt

    def begin(self):
        self.folder.mkdir(parents=True)
        sealed_path = self.case['placement_receipt']
        digest = generic.sha(sealed_path)
        if digest != self.case['placement_receipt_sha256'] or self.pinned.get(str(Path(sealed_path).resolve())) != digest:
            raise EnvironmentFailure('sealed placement receipt changed since sealing')
        try:
            self.gate.begin_case(self.folder, 'status-case-before')
        finally:
            cg.persist_receipts(self.gate, self.folder, 'before')
        sealed = json.loads(Path(sealed_path).read_text())
        self.before = self.placement('before')
        check_fresh_receipt(self.before, self.case, sealed, self.folder / 'compare-sealed-before.json')

    def end(self):
        """Placement and status after the last process; both are taken even if one fails."""
        failures = []
        try:
            check_fresh_receipt(self.placement('after'), self.case, self.before, self.folder / 'compare-before-after.json')
        except EnvironmentFailure as exc:
            failures.append(exc)
        try:
            self.gate.check(self.folder, 'status-case-after')
        except EnvironmentFailure as exc:
            failures.append(exc)
        finally:
            cg.persist_receipts(self.gate, self.folder, 'after')
        if failures:
            raise failures[0]


def default_spawn(command, stdout, stderr, env):
    return subprocess.Popen(command, stdout=stdout, stderr=stderr, env=env, start_new_session=True)


def run(args, command=None, helper=None, spawn=default_spawn, sleep=time.sleep, clock=time.monotonic, utc=cg.utc_now):
    output = Path(args.output)
    mc.require_outside_repo(output, 'testing.B output')
    manifest_path = Path(args.manifest).resolve()
    if generic.sha(manifest_path) != args.manifest_sha256:
        raise EnvironmentFailure('sealing manifest differs from the acknowledged SHA-256')
    manifest = json.loads(manifest_path.read_text())
    entry = next((p for p in manifest['phases'] if p['phase'] == args.phase), None)
    plan_path = manifest_path.parent / f'{args.phase}.plan.json'
    plan_hash = generic.sha(plan_path)
    if entry is None or entry['plan_sha256'] != plan_hash:
        raise EnvironmentFailure('plan differs from its sealing manifest entry')
    plan = json.loads(plan_path.read_text())
    protocol_path, orders_path = Path(args.protocol).resolve(), Path(args.orders).resolve()
    layout_path, receipt_path = Path(args.layout).resolve(), Path(args.source_receipt).resolve()
    helper_path = (Path(args.scripts) / 'run.py').resolve()
    pinned = manifest['pinned_files']
    template = json.loads(args.placement_command)
    if not (isinstance(template, list) and template and all(isinstance(a, str) for a in template)):
        raise ValueError('--placement-command must be a JSON list of strings')
    template_files = {str(Path(a).resolve()) for a in template if a.startswith('/') and Path(a).is_file()}
    required = cg.code_files(cc, pr, sm, sys.modules[__name__]) | template_files | {
        str(p) for p in (protocol_path, orders_path, layout_path, receipt_path, helper_path)}
    missing = sorted(required - set(pinned))
    if missing:
        raise EnvironmentFailure(f'runner code, inputs and placement command must be pinned in the manifest: {missing}')
    protocol, orders = json.loads(protocol_path.read_text()), json.loads(orders_path.read_text())
    layout = json.loads(layout_path.read_text())
    phase = rebuild(plan, protocol, orders, args.phase)
    if not plan['engine'] == phase['engine'] == layout['engine']:
        raise EnvironmentFailure('plan, phase and layout engines differ')
    if not plan['placement_required']:
        raise EnvironmentFailure('multi-node testing.B phases require placement')
    policy = cg.client_vm_policy(protocol)
    cases = {c['key']: c for c in plan['cases']}
    last_job = {j['case']: j['id'] for j in plan['jobs']}
    binary = plan['jobs'][0]['command'][3]
    cluster = plan['env']['FDB_BENCH_CLUSTER_FILE']
    harness = (protocol['new_harness_revision'] if any(c['cell'] in phase.get('new_harness_cells', []) for c in plan['cases'])
               or phase.get('new_harness_phase') else protocol['frozen_harness_revision'])
    output.mkdir(parents=True, exist_ok=False)
    active = None
    samples, results, omitted = [], [], {}
    deadline = time.monotonic() + plan['phase_wall_cap_seconds']
    env = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'HOME': str(Path.home()), **plan['env']}
    handlers = {}

    def verify_all():
        generic.verify_pins(plan)
        if {p: generic.sha(p) for p in pinned} != pinned:
            raise EnvironmentFailure('pinned runner/config/receipt inputs changed')

    try:
        left = cg.lifetime_left(args.lifetime_deadline)
        need = plan['phase_wall_cap_seconds'] + cg.LIFETIME_RESERVE_SECONDS
        if left < need:
            generic.dump(output / 'UNEXECUTED.json', dict(reason='budget', remaining_seconds=left, required_seconds=need))
            raise EnvironmentFailure('insufficient lifetime for the phase cap, collection and teardown verification')
        pins = generic.verify_pins(plan)
        verify_all()
        if not harness:
            raise EnvironmentFailure('new-harness cases need protocol new_harness_revision')
        generic.validate_source_receipt(json.loads(receipt_path.read_text()),
                                       dict(harness_commit=harness, production_revisions=protocol['production_revisions'],
                                            expected_sha256=pinned))
        with open(binary, 'rb') as stream:
            if stream.read(4) != b'\x7fELF' or not os.access(binary, os.X_OK):
                raise EnvironmentFailure('required compiled executable missing: ' + binary)
        generic.dump(output / 'pins.before.json', pins)
        generic.dump(output / 'plan.json', plan)
        generic.dump(output / 'controller.json', dict(plan_sha256=plan_hash, manifest_sha256=args.manifest_sha256,
                                                    python=sys.version, argv=sys.argv, lifetime_deadline=args.lifetime_deadline,
                                                    environment=env, placement_command=template, harness_revision=harness,
                                                    policy=policy))
        housekeeping = generic.cpu_set(policy, 'housekeeping')
        cg.cluster_file_guard(cluster, layout, args.vpc_cidr)
        cg.affinity_guard(housekeeping)
        helper = helper or generic.load_helper(helper_path)
        command = command or cg.default_command(helper, env)
        gate = cg.ServerGate(cluster, layout, plan['engine'], 'disabled', command, sleep=sleep, clock=clock)

        def interrupted(sig, frame):
            raise KeyboardInterrupt('signal ' + str(sig))
        handlers = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
        current = None
        for job in plan['jobs']:
            key = job['case']
            if key in omitted:
                continue
            if current is None or current.case['key'] != key:
                current = Case(cases[key], output / 'cases' / key, gate, command, template, cluster, pinned)
                try:
                    current.begin()
                except EnvironmentFailure as exc:
                    omitted[key] = dict(reason='environment-aborted before the case: ' + str(exc), at=job['id'],
                                        placement_label=cases[key]['placement_label'])
                    generic.dump(output / 'omitted.json', omitted)
                    print(f'{key} omitted before any process: {exc}', flush=True)
                    continue
            folder = output / job['id']
            folder.mkdir()
            generic.dump(folder / 'invocation.json', job)
            view = dict(job, case=audit_case(cases[key]))
            result = dict(id=job['id'], run_id=job['id'], case=key, block=job['block'], position=job['position'], arm=job['arm'],
                          placement_label=cases[key]['placement_label'], status='started')
            results.append(result)
            try:
                if time.monotonic() + 3 >= deadline:
                    raise EnvironmentFailure('phase wall cap before next arm')
                verify_all()
                cg.cluster_file_guard(cluster, layout, args.vpc_cidr)
                cg.affinity_guard(housekeeping)
                ensure_idle(helper, binary)
                gate.pre_arm(folder, 'status-before')
                sleep(policy['cooldown_seconds'])
                quiet = helper.quiescence(policy)
                generic.dump(folder / 'quiescence.json', quiet)
                if not quiet['passed']:
                    raise EnvironmentFailure('pre-arm CPU/steal gate failed')
                ensure_idle(helper, binary)
                helper.snapshot(folder / 'before')
                started = time.monotonic_ns()
                result['started_utc'] = utc()
                telemetry = []
                with open(folder / 'stdout.txt', 'x') as out, open(folder / 'stderr.txt', 'x') as err, \
                        open(folder / 'telemetry.jsonl', 'x') as stream:
                    active = spawn(['/usr/bin/time', '-v', '-o', str(folder / 'process-time.txt'), '--', *job['command']],
                                   out, err, env)
                    process_deadline = min(deadline, time.monotonic() + plan['process_timeout_seconds'])
                    while True:
                        sample = helper.telemetry(active.pid)
                        telemetry.append(sample)
                        stream.write(json.dumps(sample) + '\n')
                        stream.flush()
                        if time.monotonic() >= process_deadline:
                            raise EnvironmentFailure('benchmark process timeout; no retry')
                        ensure_idle(helper, binary, helper.descendants(active.pid))
                        remaining = process_deadline - time.monotonic()
                        if remaining <= 0:
                            raise EnvironmentFailure('process timeout after telemetry')
                        try:
                            active.wait(timeout=min(1, remaining))
                            break
                        except subprocess.TimeoutExpired:
                            pass
                    telemetry.append(helper.telemetry(active.pid))
                    stream.write(json.dumps(telemetry[-1]) + '\n')
                rc = active.returncode
                helper.stop_process_group(active)
                active = None
                result['ended_utc'] = utc()
                time_text = (folder / 'process-time.txt').read_text()
                cpu = {}
                # `metric`, not `key`: `key` is this job's case key, written to every sample below.
                for field, metric in [('User time (seconds)', 'process_user_cpu_seconds'),
                                      ('System time (seconds)', 'process_system_cpu_seconds')]:
                    match = re.search(re.escape(field) + r':\s*(\S+)', time_text)
                    if not match:
                        raise EnvironmentFailure('missing GNU time lifetime CPU receipt')
                    cpu[metric] = generic.number(float(match.group(1)))
                result.update(cpu)
                result.update(returncode=rc, process_wall_ns=time.monotonic_ns() - started,
                              cpu_scope='GNU time user/system CPU includes fixtures/calibration/audits/shutdown; NOT CPU/op')
                helper.snapshot(folder / 'after')
                cg.telemetry_guard(telemetry, policy)
                if cc.oom_count((folder / 'before/proc_vmstat').read_text()) != cc.oom_count((folder / 'after/proc_vmstat').read_text()):
                    raise EnvironmentFailure('OOM during arm')
                gate.check(folder, 'status-after')
                verify_all()
                if rc:
                    stderr = (folder / 'stderr.txt').read_text()
                    if rc < 0 or 'signal' in stderr or 'timeout' in stderr.lower():
                        raise EnvironmentFailure('benchmark signalled/environmental failure')
                    raise CorrectnessFailure('benchmark nonzero exit; raw logs retained')
                rows, audits = generic.parse_output((folder / 'stdout.txt').read_text() + '\n' + (folder / 'stderr.txt').read_text(),
                                                    view, repeats=phase['repeats_per_arm_process'],
                                                    gomaxprocs=plan['env']['GOMAXPROCS'])
                generic.dump(folder / 'audits.json', audits)
                for row in rows:
                    samples.append(dict(phase=plan['phase'], case=key, arm=job['arm'], block=job['block'],
                                        position=job['position'], repeat=row['repeat'], N=row['N'], **row['metrics']))
                result['status'] = 'ok'
                generic.dump(folder / 'parsed.json', rows)
                if last_job[key] == job['id']:
                    current.end()
            except EnvironmentFailure as exc:
                helper.stop_process_group(active)
                active = None
                if 'started_utc' in result and 'ended_utc' not in result:
                    result['ended_utc'] = utc()  # the process group is gone by now
                if result['status'] == 'ok':  # the case-end placement/status check failed
                    omitted[key] = dict(reason='contaminated after the last process: ' + str(exc), at=job['id'],
                                        placement_label=cases[key]['placement_label'])
                else:
                    result.update(status='environment-aborted', error=str(exc), contaminated_block=job['block'])
                    omitted[key] = dict(reason='environment-aborted; no ratios/no automatic retries', at=job['id'],
                                        placement_label=cases[key]['placement_label'])
            except CorrectnessFailure as exc:
                # Every multi-node testing.B arm is the final tree: a correctness failure stops the phase.
                result.update(status='correctness-failed', error=str(exc))
                omitted[key] = dict(reason='final correctness-failed; phase stopped', at=job['id'])
                raise
            finally:
                generic.retain_raw_audits(folder)
                generic.dump(folder / 'result.json', result)
                generic.dump(output / 'progress.json', results)
                generic.dump(output / 'omitted.json', omitted)
            print(job['id'] + ' ' + result['status'], flush=True)
        after = generic.verify_pins(plan)
        verify_all()
        generic.dump(output / 'pins.after.json', after)
        if generic.sha(plan_path) != plan_hash:
            raise EnvironmentFailure('plan changed during execution')
        generic.write_metrics(output / 'metrics.csv', samples)
        if time.monotonic() >= deadline:
            raise EnvironmentFailure('phase wall cap exceeded')
        if omitted:
            generic.dump(output / 'INCOMPLETE.json', dict(reason='retained adverse cases; NOT a complete measurement population',
                                                         omitted=omitted, scheduled=len(plan['jobs']), finished=len(results),
                                                         raw_samples=len(samples)))
            return 2
        repeats = phase['repeats_per_arm_process']
        if len(samples) != len(plan['jobs']) * repeats or len(results) != len(plan['jobs']) or any(r['status'] != 'ok' for r in results):
            raise CorrectnessFailure('incomplete/nonempty schedule population')
        generic.dump(output / 'complete.json', dict(scheduled=len(plan['jobs']), completed=len(results), samples=len(samples),
                                                   pins_unchanged=True, plan_sha256=plan_hash,
                                                   placement_labels={k: c['placement_label'] for k, c in cases.items()}))
        return 0
    except BaseException as exc:
        if output.is_dir():
            generic.dump(output / 'ABORTED.json', dict(error=repr(exc), results=results, omitted=omitted))
            if not (output / 'metrics.csv').exists():
                generic.write_metrics(output / 'metrics.csv', samples)
        raise
    finally:
        if helper:
            helper.stop_process_group(active)
        for sig, handler in handlers.items():
            signal.signal(sig, handler)
        if output.is_dir():
            try:
                generic.dump(output / 'pins.finally.json', {p: generic.sha(p) if Path(p).is_file() else None
                                                            for p in [*plan['expected_sha256'], *pinned]})
            except OSError as exc:
                generic.dump(output / 'pins-finally-error.json', dict(error=str(exc)))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest='command', required=True)
    p = sub.add_parser('run')
    for flag in ('manifest', 'manifest-sha256', 'phase', 'protocol', 'orders', 'layout', 'vpc-cidr', 'scripts',
                 'source-receipt', 'placement-command', 'output', 'lifetime-deadline'):
        p.add_argument('--' + flag, required=True)
    args = parser.parse_args(argv)
    return run(args)


if __name__ == '__main__':
    sys.exit(main())
