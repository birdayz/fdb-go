#!/usr/bin/env python3
"""Assemble reconcile_telemetry.py's input (fdb-bench-reconcile-input/v1) for one phase.

  build_reconcile_input.py --manifest PLANS/manifest.json --manifest-sha256 SHA --phase PHASE
      --results DIR --layout cluster-ssd/layout.json
      --client-receipt client-before.json --client-receipt client-after.json
      --node node1=DIR --node node2=DIR --node node3=DIR --output reconcile-input.json

The sealing manifest (it must hash to --manifest-sha256) decides the phase kind and
its expected cases, so no case can be left out:
  testing.B (seal_multinode testingb-plans): the sealed plan's cases. --results is
    bench_runner_mn.py's --output: each case directory cases/<case>/ holds
    status-before/after.json, check-roles-before/after.json and the placement
    compares compare-sealed-before.json and compare-before-after.json; each job's
    <job id>/result.json holds run_id, started_utc and ended_utc.
  client (seal_multinode client-plans): the phase's sealed workloads. --results is
    the phase directory of client_campaign_mn.py outputs (<results>/<workload>/):
    status/check-roles receipts as above, and run.py's <run id>/result.json per run.
--client-receipt: node_sampler.py receipt --no-units --kind before|after on the client.
--node: each layout node's node_sampler.py run --output-dir.

Refused (nothing written): a manifest, plan, layout, results directory, client
receipt or node directory that is missing, unpinned, for another phase or plan;
and a per-case receipt whose SHA-256 differs from the one its runner recorded in
case-receipts.json. Per-case evidence that was never written (a runner aborted
before taking it) is referenced at its expected path and listed under
builder.missing: reconcile_telemetry.py then reports that case unverifiable,
exactly as protocol environment_policy requires for missing data.
Every path in the output is absolute. The output must be new and outside any Git
work tree.
"""
import argparse
import hashlib
import json
from pathlib import Path
import sys

import mn_common as mc

INPUT_FORMAT = 'fdb-bench-reconcile-input/v1'  # reconcile_telemetry.INPUT_FORMAT
RECEIPTS = ('status-before.json', 'check-roles-before.json', 'status-after.json', 'check-roles-after.json')


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


def sealed_phase(manifest_path, manifest_sha256, phase):
    """-> (kind, expected case ids, placement_required, sealed detail, pinned files)."""
    manifest_path = Path(manifest_path).resolve()
    if not manifest_path.is_file() or sha256(manifest_path) != manifest_sha256:
        raise Refused('sealing manifest missing or not the acknowledged SHA-256')
    manifest = load_json(manifest_path, 'sealing manifest')
    pinned = manifest.get('pinned_files') or {}
    if 'phases' in manifest:
        entry = next((p for p in manifest['phases'] if p.get('phase') == phase), None)
        plan_path = manifest_path.parent / f'{phase}.plan.json'
        if entry is None or not plan_path.is_file() or sha256(plan_path) != entry['plan_sha256']:
            raise Refused(f'phase {phase} is not sealed in this testing.B manifest, or its plan changed')
        plan = load_json(plan_path, 'sealed plan')
        return 'testingb', [c['key'] for c in plan['cases']], bool(plan['placement_required']), \
            dict(plan=plan, plan_sha256=entry['plan_sha256']), pinned
    if 'campaigns' in manifest:
        campaigns = [c for c in manifest['campaigns'] if c.get('phase') == phase]
        if not campaigns:
            raise Refused(f'phase {phase} has no sealed client campaign in this manifest')
        for c in campaigns:
            for key in ('plan', 'schedule'):
                if not Path(c[key]).is_file() or sha256(c[key]) != c[key + '_sha256']:
                    raise Refused(f'sealed {key} of {phase}/{c["workload"]} missing or changed')
        return 'client', [c['workload'] for c in campaigns], False, \
            dict(campaigns={c['workload']: c for c in campaigns}), pinned
    raise Refused('not a seal_multinode manifest')


class Case:
    """One case's references; missing evidence is recorded, changed evidence refused."""

    def __init__(self, case_id, folder, missing):
        self.id, self.folder, self.missing = case_id, Path(folder), missing
        index_path = self.folder / 'case-receipts.json'
        self.index = load_json(index_path, 'case receipt index') if index_path.is_file() else {}
        if not index_path.is_file():
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


def testingb_cases(results, sealed, missing):
    plan = sealed['plan']
    controller = load_json(results / 'controller.json', 'runner controller receipt')
    if controller.get('plan_sha256') != sealed['plan_sha256'] or load_json(results / 'plan.json', 'runner plan') != plan:
        raise Refused(f'{results} is not the output of the sealed {plan["phase"]} plan')
    out = []
    for c in plan['cases']:
        folder = results / 'cases' / c['key']
        if not folder.is_dir():
            missing.append(f'{c["key"]}: never started ({folder})')
            continue
        case = Case(c['key'], folder, missing)
        entry = case.entry()
        entry['placement_compares'] = [case.ref('compare-sealed-before.json', recorded=False),
                                       case.ref('compare-before-after.json', recorded=False)]
        entry['run_results'] = []
        for job in plan['jobs']:
            if job['case'] != c['key']:
                continue
            result = results / job['id'] / 'result.json'
            if result.is_file() and 'started_utc' in load_json(result, 'job result'):
                entry['run_results'].append(str(result))
            elif (results / job['id']).is_dir() and not result.is_file():
                missing.append(f'{c["key"]}: {result}')
                entry['run_results'].append(str(result))
        out.append(entry)
    return out


def client_cases(results, sealed, missing):
    out = []
    for workload, campaign in sealed['campaigns'].items():
        folder = results / workload
        if not folder.is_dir():
            missing.append(f'{workload}: never started ({folder})')
            continue
        adapter = load_json(folder / 'adapter.json', 'adapter receipt')
        if adapter.get('plan_sha256') != campaign['plan_sha256'] or adapter.get('schedule_sha256') != campaign['schedule_sha256']:
            raise Refused(f'{folder} is not the output of the sealed {campaign["phase"]}/{workload} plan')
        entry = Case(workload, folder, missing).entry()
        entry['run_results'], entry['runs'] = [], []
        for run in load_json(folder / 'schedule.json', 'runtime schedule'):
            result = folder / run['run_id'] / 'result.json'
            if result.is_file():
                entry['run_results'].append(str(result))
            elif (folder / run['run_id']).is_dir():
                # Started, but run.py wrote no window: unverifiable, never dropped.
                missing.append(f'{workload}: {result}')
                entry['runs'].append(dict(run_id=run['run_id'], start_utc=None, end_utc=None))
        out.append(entry)
    return out


def build(args):
    kind, expected, placement_required, sealed, pinned = sealed_phase(args.manifest, args.manifest_sha256, args.phase)
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
    cases = testingb_cases(results, sealed, missing) if kind == 'testingb' else client_cases(results, sealed, missing)
    return dict(format=INPUT_FORMAT, phase=args.phase, layout=str(layout_path), placement_required=placement_required,
                client=dict(host=args.client_host, receipts=[str(r) for r in receipts]),
                nodes={n: str(p) for n, p in sorted(nodes.items())}, expected_cases=expected, cases=cases,
                builder=dict(sha256=sha256(__file__), kind=kind, manifest=str(Path(args.manifest).resolve()),
                             manifest_sha256=args.manifest_sha256, results=str(results), missing=missing))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    for flag in ('manifest', 'manifest-sha256', 'phase', 'results', 'layout', 'output'):
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
    print(f'{args.phase}: {len(doc["expected_cases"])} expected cases, {len(doc["cases"])} with receipts, '
          f'{len(doc["builder"]["missing"])} missing pieces (reconciliation reports them unverifiable)')
    for item in doc['builder']['missing']:
        print('MISSING ' + item)
    return 0


if __name__ == '__main__':
    sys.exit(main())
