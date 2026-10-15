#!/usr/bin/env python3
"""Assemble reconcile_telemetry.py's input (fdb-bench-reconcile-input/v1) for one phase.

  build_reconcile_input.py --manifest PLANS/manifest.json --manifest-sha256 SHA --phase PHASE
      --protocol protocol.json --results DIR --layout cluster-ssd/layout.json
      --client-receipt client-before.json --client-receipt client-after.json
      --node node1=DIR --node node2=DIR --node node3=DIR --output reconcile-input.json

Everything expected comes from the acknowledged seal, never from what exists on
disk: the manifest must hash to --manifest-sha256, and its sealed population must
be the protocol's phase population minus exactly the manifest's recorded unsealed
(new-harness) items.
  testing.B (seal_multinode testingb-plans): expected cases and runs are the sealed
    plan's cases and jobs. --results is bench_runner_mn.py's --output.
  client (seal_multinode client-plans): expected cases are the sealed workloads,
    expected runs each campaign's sealed schedule.json (path and SHA-256 from the
    manifest). --results is the phase directory of client_campaign_mn.py outputs.
Every expected case gets an entry and every expected run a window: run.py's or
bench_runner_mn.py's result.json (run_id, started_utc, ended_utc), or, when it is
absent or holds no window, an explicit empty window that reconcile_telemetry.py
(R4) reports unverifiable. Each case also carries:
  status_before/after, roles_before/after (status-*.json, check-roles-*.json);
  placement_compares (testing.B: compare-sealed-before.json, compare-before-after.json);
  runner_verdict {omitted, adverse, source, source_sha256}: the runner's own record.
    R2: omitted.json plus every sealed job's recorded result status (any job not
    'ok' is adverse; a job without result.json is a missing window, not a verdict). R1: client-phase-state.json's entry for the workload, its
    ADAPTER_ADVERSE/ADAPTER_PREFLIGHT_ADVERSE/ABORTED/UNEXECUTED files and run.py's
    complete.json. A missing or unreadable record gives omitted null (R4: unverifiable).
The phase carries expected_dd_state: frozen for testing.B (M2), on for client (M1).
Missing pieces are referenced at their expected path and listed under builder.missing.

Refused (nothing written): a manifest, plan, schedule, protocol or layout that is
missing, unacknowledged, unpinned or inconsistent with the seal; a results
directory, runtime schedule or result.json that belongs to another plan or run; a
per-case receipt whose SHA-256 differs from the one its runner recorded in
case-receipts.json; a missing client receipt or node telemetry directory; an output
that exists or lies inside a Git work tree. Every path in the output is absolute.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import cluster_gate as cg  # noqa: E402  (sealed populations; puts the single-node modules on the path)
import client_campaign as cc  # noqa: E402  (single-node: normalize_schedule)
import mn_common as mc  # noqa: E402

INPUT_FORMAT = 'fdb-bench-reconcile-input/v1'  # reconcile_telemetry.INPUT_FORMAT
DD_STATE = {'testingb': 'frozen', 'client': 'on'}
R1_ADVERSE = ('ADAPTER_ADVERSE.json', 'ADAPTER_PREFLIGHT_ADVERSE.json', 'ABORTED.json', 'UNEXECUTED.json')


class Refused(Exception):
    pass


def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def load_json(path, what):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError) as exc:
        raise Refused(f'{what} {path} missing or unreadable: {exc}') from None


def sealed_phase(manifest_path, manifest_sha256, phase_id, protocol_path):
    """-> (kind, protocol phase, expected case ids, sealed detail, pinned files)."""
    manifest_path, protocol_path = Path(manifest_path).resolve(), Path(protocol_path).resolve()
    if not manifest_path.is_file() or sha256(manifest_path) != manifest_sha256:
        raise Refused('sealing manifest missing or not the acknowledged SHA-256')
    manifest = load_json(manifest_path, 'sealing manifest')
    pinned = manifest.get('pinned_files') or {}
    if not protocol_path.is_file():
        raise Refused(f'protocol missing: {protocol_path}')
    digest = sha256(protocol_path)
    if digest not in (manifest.get('protocol_sha256'), pinned.get(str(protocol_path))):
        raise Refused('protocol is not the one the manifest sealed')
    protocol = load_json(protocol_path, 'protocol')
    phase = next((p for p in protocol['phases'] if p['id'] == phase_id), None)
    if phase is None:
        raise Refused(f'phase {phase_id} is not in the protocol')
    try:
        if 'phases' in manifest:
            entry = next((p for p in manifest['phases'] if p.get('phase') == phase_id), None)
            plan_path = manifest_path.parent / f'{phase_id}.plan.json'
            if entry is None or not plan_path.is_file() or sha256(plan_path) != entry['plan_sha256']:
                raise Refused(f'phase {phase_id} is not sealed in this testing.B manifest, or its plan changed')
            plan = load_json(plan_path, 'sealed plan')
            expected = cg.sealed_testingb_cases(manifest, phase, plan)
            return 'testingb', phase, expected, dict(plan=plan, plan_sha256=entry['plan_sha256']), pinned
        if 'campaigns' in manifest:
            expected = cg.sealed_client_workloads(manifest, phase)
            campaigns = {c['workload']: c for c in manifest['campaigns'] if c.get('phase') == phase_id}
            for c in campaigns.values():
                for key in ('plan', 'schedule'):
                    if not Path(c[key]).is_file() or sha256(c[key]) != c[key + '_sha256']:
                        raise Refused(f'sealed {key} of {phase_id}/{c["workload"]} missing or changed')
            return 'client', phase, expected, dict(campaigns=campaigns), pinned
    except ValueError as exc:
        raise Refused(str(exc)) from None
    raise Refused('not a seal_multinode manifest')


class Case:
    """One case's references; missing evidence is recorded, changed evidence refused."""

    def __init__(self, case_id, folder, missing):
        self.id, self.folder, self.missing = case_id, Path(folder), missing
        index_path = self.folder / 'case-receipts.json'
        self.indexed = index_path.is_file()
        self.index = load_json(index_path, 'case receipt index') if self.indexed else {}
        if not self.indexed:
            missing.append(f'{case_id}: {index_path}')

    def ref(self, name, recorded=True):
        path = self.folder / name
        if not path.is_file():
            self.missing.append(f'{self.id}: {path}')
        elif recorded and self.index.get(name) != sha256(path):
            raise Refused(f'{path} differs from the SHA-256 its runner recorded')
        return str(path)

    def entry(self):
        return dict(case=self.id, status_before=self.ref('status-before.json'),
                    roles_before=self.ref('check-roles-before.json'), status_after=self.ref('status-after.json'),
                    roles_after=self.ref('check-roles-after.json'))


def windows(case_id, run_ids, result_of, missing):
    """Every expected run: its result.json if it holds a window, else an explicit empty window."""
    results, runs = [], []
    for run_id in run_ids:
        path = result_of(run_id)
        value = load_json(path, 'run result') if path.is_file() else None
        if value is not None and value.get('run_id') != run_id:
            raise Refused(f'{path} is the result of {value.get("run_id")!r}, not {run_id!r}')
        if value is not None and value.get('started_utc') and value.get('ended_utc'):
            results.append(str(path))
        else:
            missing.append(f'{case_id}: no run window for {run_id} ({path})')
            runs.append(dict(run_id=run_id, start_utc=None, end_utc=None))
    return results, runs


def null_verdict(source, missing, why):
    missing.append(f'runner verdict: {why} ({source})')
    return dict(omitted=None, adverse=[], source=str(source), source_sha256=None)


def r2_verdict(results, plan, key, missing):
    source = results / 'omitted.json'
    try:
        omitted = json.loads(source.read_text())
        digest = sha256(source)
    except (OSError, ValueError) as exc:
        return null_verdict(source, missing, f'R2 omitted.json missing or unreadable: {type(exc).__name__}')
    if not isinstance(omitted, dict):
        return null_verdict(source, missing, 'R2 omitted.json is not an object')
    adverse = []
    if key in omitted:
        adverse.append('runner omitted the case: ' + str((omitted[key] or {}).get('reason')))
    for job in plan['jobs']:
        if job['case'] != key:
            continue
        path = results / job['id'] / 'result.json'
        if not path.is_file():
            continue  # no runner verdict to read: its missing window already makes the case unverifiable
        result = load_json(path, 'job result')
        if result.get('status') != 'ok':
            adverse.append(f'{job["id"]}: {result.get("status")} {result.get("error", "")}'.rstrip())
    return dict(omitted=key in omitted or bool(adverse), adverse=adverse, source=str(source), source_sha256=digest)


def r1_verdict(results, folder, workload, missing):
    source = results / 'client-phase-state.json'
    try:
        state = json.loads(source.read_text())
        digest = sha256(source)
        entry = state['cases'][workload]
        status = entry['status']
    except (OSError, ValueError, KeyError, TypeError) as exc:
        return null_verdict(source, missing, f'R1 phase state has no record of {workload}: {type(exc).__name__}')
    adverse = []
    if status != 'complete':
        adverse.append(f'phase state: {status}: {entry.get("error", "")}'.rstrip(': '))
    for name in R1_ADVERSE:
        if (folder / name).is_file():
            adverse.append(f'{name}: {(folder / name).read_text()[:300]}')
    if status == 'complete' and not (folder / 'complete.json').is_file():
        adverse.append('run.py complete.json missing')
    return dict(omitted=bool(adverse), adverse=adverse, source=str(source), source_sha256=digest)


def testingb_cases(results, sealed, missing):
    plan = sealed['plan']
    controller = load_json(results / 'controller.json', 'runner controller receipt')
    if controller.get('plan_sha256') != sealed['plan_sha256'] or load_json(results / 'plan.json', 'runner plan') != plan:
        raise Refused(f'{results} is not the output of the sealed {plan["phase"]} plan')
    out = []
    for c in plan['cases']:
        key = c['key']
        folder = results / 'cases' / key
        if not folder.is_dir():
            missing.append(f'{key}: case directory missing ({folder})')
        case = Case(key, folder, missing)
        entry = case.entry()
        entry['placement_compares'] = [case.ref('compare-sealed-before.json', recorded=False),
                                       case.ref('compare-before-after.json', recorded=False)]
        jobs = [j['id'] for j in plan['jobs'] if j['case'] == key]
        entry['run_results'], entry['runs'] = windows(key, jobs, lambda j: results / j / 'result.json', missing)
        entry['runner_verdict'] = r2_verdict(results, plan, key, missing)
        out.append(entry)
    return out


def client_cases(results, sealed, expected, missing):
    out = []
    for workload in expected:
        campaign = sealed['campaigns'][workload]
        folder = results / workload
        schedule = load_json(campaign['schedule'], 'sealed schedule')
        if not folder.is_dir():
            missing.append(f'{workload}: campaign directory missing ({folder})')
        else:
            adapter = load_json(folder / 'adapter.json', 'adapter receipt')
            if adapter.get('plan_sha256') != campaign['plan_sha256'] or adapter.get('schedule_sha256') != campaign['schedule_sha256']:
                raise Refused(f'{folder} is not the output of the sealed {campaign["phase"]}/{workload} plan')
            runtime = folder / 'schedule.json'
            if runtime.is_file() and cc.normalize_schedule(load_json(runtime, 'runtime schedule')) != cc.normalize_schedule(schedule):
                raise Refused(f'{runtime} differs from the sealed schedule {campaign["schedule"]}')
            if not runtime.is_file():
                missing.append(f'{workload}: runtime schedule missing ({runtime})')
        entry = Case(workload, folder, missing).entry()
        entry['run_results'], entry['runs'] = windows(workload, [r['run_id'] for r in schedule],
                                                      lambda r: folder / r / 'result.json', missing)
        entry['runner_verdict'] = r1_verdict(results, folder, workload, missing)
        out.append(entry)
    return out


def build(args):
    kind, phase, expected, sealed, pinned = sealed_phase(args.manifest, args.manifest_sha256, args.phase, args.protocol)
    layout_path = Path(args.layout).resolve()
    if not layout_path.is_file() or pinned.get(str(layout_path)) != sha256(layout_path):
        raise Refused(f'layout {layout_path} missing or not the one pinned in the sealing manifest')
    layout = load_json(layout_path, 'layout')
    if not isinstance(layout, dict) or not isinstance(layout.get('processes'), list) or not layout['processes'] or \
            not all(isinstance(p, dict) and p.get('node') for p in layout['processes']):
        raise Refused(f'{layout_path} is not an fdb_cluster.py layout')
    nodes = {}
    for spec in args.node:
        name, sep, path = spec.partition('=')
        if not sep or name in nodes:
            raise Refused('--node must be unique NAME=DIR: ' + spec)
        nodes[name] = Path(path).resolve()
    want = sorted({p['node'] for p in layout['processes']})
    if sorted(nodes) != want:
        raise Refused(f'--node names {sorted(nodes)} differ from layout nodes {want}')
    for name, path in nodes.items():
        if not path.is_dir():
            raise Refused(f'telemetry directory of {name} missing: {path}')
    receipts = [Path(r).resolve() for r in args.client_receipt]
    for r in receipts:
        if not r.is_file():
            raise Refused(f'client receipt missing: {r}')
    results = Path(args.results).resolve()
    if not results.is_dir():
        raise Refused(f'results directory missing: {results}')
    missing = []
    cases = (testingb_cases(results, sealed, missing) if kind == 'testingb'
             else client_cases(results, sealed, expected, missing))
    if [c['case'] for c in cases] != expected:
        raise Refused('internal: case entries differ from the sealed population')
    return dict(format=INPUT_FORMAT, phase=args.phase, layout=str(layout_path),
                placement_required=bool(phase.get('placement_required', False)), expected_dd_state=DD_STATE[kind],
                client=dict(host=args.client_host, receipts=[str(r) for r in receipts]),
                nodes={n: str(p) for n, p in sorted(nodes.items())}, expected_cases=expected, cases=cases,
                builder=dict(sha256=sha256(__file__), kind=kind, manifest=str(Path(args.manifest).resolve()),
                             manifest_sha256=args.manifest_sha256, protocol=str(Path(args.protocol).resolve()),
                             results=str(results), missing=missing))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for flag in ('manifest', 'manifest-sha256', 'phase', 'protocol', 'results', 'layout', 'output'):
        ap.add_argument('--' + flag, required=True)
    ap.add_argument('--client-receipt', action='append', required=True, help='client chrony receipt (repeat)')
    ap.add_argument('--client-host', default='client', help='host name the client receipts were taken as')
    ap.add_argument('--node', action='append', required=True, help='NAME=node_sampler run directory (repeat)')
    args = ap.parse_args(argv)
    try:
        mc.require_outside_repo(args.output, 'reconcile input')
        doc = build(args)
        mc.dump_json(args.output, doc)
    except (Refused, mc.GuardError, FileExistsError) as exc:
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2
    print(f'{args.phase}: {len(doc["expected_cases"])} expected cases, {len(doc["builder"]["missing"])} missing '
          'pieces (reconciliation reports their cases unverifiable)')
    for item in doc['builder']['missing']:
        print('MISSING ' + item)
    return 0


if __name__ == '__main__':
    sys.exit(main())
