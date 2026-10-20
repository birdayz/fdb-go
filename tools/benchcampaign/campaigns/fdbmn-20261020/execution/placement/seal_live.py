#!/usr/bin/env python3
"""Seal four fixed live/diagnostic modes on the campaign client. Runs no workload.

Uses the unchanged seal_multinode.py. No SSH, cloud, or cluster commands.
Diagnostics get separate protocol/orders/results paths; live protocol is read-only.
Exit 0: exact requested population sealed; 2: refused/failed, never run results.
"""
import argparse
import contextlib
import copy
import importlib
import io
import json
import os
from pathlib import Path
import re
import sys

from p0 import Failure, HARNESS, NEW, OLD, dump, load, mkdir, need, safe, sha

MODES = ('client-diagnostic', 'client-final', 'testingb-diagnostic', 'testingb-final')
STORES = {'benchfetch/' + n for n in ('small', 'split', 'planskip')}
OVERLAYS = ('client-harness', 'common', 'final-only', 'harness-doc', 'remote')
CLEANUP_COMMIT = '066920ebb8d22c5618b31ef0a56fa7674c773cdb'


def diagnostic_protocol(original, mode):
    p = copy.deepcopy(original)
    client = mode == 'client-diagnostic'
    phase = copy.deepcopy(next(x for x in p['phases'] if x['id'] == ('M1w1' if client else 'M2skip')))
    phase['blocks'] = 2
    phase['nominal_observations_per_arm_per_case'] = 2
    phase['claim_scope'] = 'Qualification diagnostic only; not campaign measurements or reported arm ratios.'
    if client:
        phase.update(arms=['final-Go', 'CGo-7.3.77'], workloads=['grv'], new_harness_workloads=[], workers=1,
                     iterations=32, iterations_per_worker=32, warmup=4, warmup_per_worker=4)
    else:
        phase.update(cells=['rows10'], dirs=['forward'], new_harness_cells=[], repeats_per_arm_process=1,
                     benchtime='1x')
    p['phases'] = [phase]
    p['campaign'] += '-diagnostic-' + ('m1' if client else 'm2')
    p['status'] = 'QUALIFICATION DIAGNOSTIC ONLY; not a campaign measurement'
    p['phase_order'] = {'default': [phase['id']], 'rationale': 'Isolated diagnostic; never mixed into final results.'}
    p['diagnostic'] = {'mode': mode, 'not_campaign_results': True,
                       'source_production_revisions_unchanged': True}
    return p


def check_revisions(protocol, source):
    expected = {'before_allocation_work_after_read_parity': OLD, 'final': NEW}
    need(protocol['production_revisions'] == expected, 'live production revision object mismatch')
    need(protocol['frozen_harness_revision'] == HARNESS, 'live frozen harness mismatch')
    need(source['production_revisions'] == protocol['production_revisions'], 'source production object differs from protocol')
    need(source['harness_commit'] == HARNESS, 'source harness mismatch')


def harness_compatibility(root, protocol, source):
    declared = 'harness_compatibility' in protocol
    need(declared == ('harness_compatibility' in source), 'protocol/source compatibility declaration presence differs')
    if not declared:
        return None
    compat = protocol['harness_compatibility']
    need(isinstance(compat, dict) and compat == source['harness_compatibility'],
         'protocol/source harness_compatibility metadata differs')
    need(set(compat) == {'description', 'upstream_commit', 'client_harness_archive', 'patch_file',
                         'patch_sha256', 'archive_sha256'}, 'compatibility metadata must have the exact 6 declared fields')
    need(isinstance(compat['description'], str) and bool(compat['description'].strip()), 'compatibility description is empty')
    need(compat['upstream_commit'] == CLEANUP_COMMIT, 'unexpected cleanup compatibility upstream commit')
    for path_key, hash_key, relative in (
            ('client_harness_archive', 'archive_sha256', 'frozen/cleanup-compat/client-harness.tar.gz'),
            ('patch_file', 'patch_sha256', 'frozen/cleanup-compat/combined.patch')):
        path = root / relative
        need(compat[path_key] == str(path), 'compatibility path differs from campaign root: ' + path_key)
        need(isinstance(compat[hash_key], str) and re.fullmatch('[0-9a-f]{64}', compat[hash_key]) is not None and
             sha(path) == compat[hash_key], 'compatibility file hash mismatch: ' + path_key)
    return compat


def artifact_pins(root, source, compat=None):
    expected = {root / 'sources/master.tar.gz', root / 'sources/previous.tar.gz'} | {
        root / f'frozen/overlays/{name}.tar.gz' for name in OVERLAYS}
    if compat is not None:
        expected |= {Path(compat['client_harness_archive']), Path(compat['patch_file'])}
    recorded = source.get('source_artifacts_sha256')
    need(isinstance(recorded, dict) and set(recorded) == {str(p) for p in expected},
         'source receipt must name exactly ' + str(len(expected)) + ' artifacts (2 sources + 5 original overlays' +
         (' + 2 declared compatibility files)' if compat is not None else ')') + ' at actual root paths')
    for name, digest in recorded.items():
        need(Path(name).is_absolute() and sha(name) == digest, 'source artifact hash mismatch: ' + name)
    return expected


def p0_inputs(root, p0root):
    p0root = safe(p0root)
    complete = load(p0root / 'complete.json')
    need(complete.get('ok') is True and complete.get('integral') is True and
         complete.get('exit_status') in (0, 3) and complete.get('dd_left_off') is True,
         'P0 is incomplete, non-integral, or did not leave DD off')
    need(sha(root / 'cluster-ssd/fdb.cluster') == complete['cluster_file_sha256'], 'P0 cluster file changed')
    need(sha(root / 'bin/placementprobe-cgo') == complete['placementprobe_sha256'], 'P0 PP binary changed')
    pins = load(p0root / 'seal-pins.json')
    need(isinstance(pins, dict) and bool(pins), 'P0 seal-pins empty')
    for name, digest in pins.items():
        need(Path(name).is_absolute() and sha(name) == digest, 'P0 sealing input changed: ' + name)
    selected = load(p0root / 'selected-receipts.json')
    fixtures = load(p0root / 'fixture-receipts.json')
    need(set(selected) == STORES and set(fixtures) == STORES, 'P0 must cover all 3 exact stores')
    need(set(selected.values()) <= set(pins) and set(fixtures.values()) <= set(pins), 'P0 missing selected receipt/fixture pins')
    need(complete['selected_receipts'] == selected, 'P0 complete/selected receipts disagree')
    need(sha(p0root / 'placement-command.json') == complete['placement_command_sha256'], 'P0 placement command changed')
    for store, path in selected.items():
        need(load(path).get('store') == store, 'P0 selected receipt/store mismatch')
    return {Path(x) for x in pins} | {p0root / 'seal-pins.json', p0root / 'complete.json'}, selected


def validate_manifest(mode, manifest, out, protocol):
    need(manifest.get('unsealed') == [], 'sealer left unsealed work')
    if mode.startswith('client'):
        campaigns = manifest.get('campaigns', [])
        if mode == 'client-diagnostic':
            need(len(campaigns) == 1 and campaigns[0]['phase'] == 'M1w1' and campaigns[0]['workload'] == 'grv' and
                 campaigns[0]['runs'] == 4 and campaigns[0]['iterations'] == 32 and campaigns[0]['warmup'] == 4 and
                 campaigns[0]['blocks'] == 2, 'client diagnostic population differs from 1 grv x 4 processes')
        else:
            need(len(campaigns) == 28 and all(c['runs'] == 18 and c['blocks'] == 6 for c in campaigns),
                 'client final must have 28 campaigns x 18 processes')
            need({c['phase'] for c in campaigns} == {'M1w1', 'M1w8', 'M1w32', 'M1w64'}, 'wrong final client phases')
        for c in campaigns:
            need(c.get('run') is not None, 'client plan lacks R1 adapter run command')
            need(sha(c['plan']) == c['plan_sha256'] and sha(c['schedule']) == c['schedule_sha256'], 'plan/schedule hash mismatch')
            plan = load(c['plan'])
            expected_arms = ['final', 'cgo'] if mode == 'client-diagnostic' else ['before', 'final', 'cgo']
            need([a['name'] for a in plan['arms']] == expected_arms, 'wrong client plan arm population')
        return {c['plan']: c['plan_sha256'] for c in campaigns} | {c['schedule']: c['schedule_sha256'] for c in campaigns}
    expected = {'M2skip': (1, 4)} if mode == 'testingb-diagnostic' else {'M2rl': (12, 144), 'M2skip': (4, 48)}
    phases = manifest.get('phases', [])
    need({p['phase']: (p['cases'], p['jobs']) for p in phases} == expected and len(phases) == len(expected),
         'testing.B case/process population differs')
    plan_hashes = {}
    for entry in phases:
        path = out / (entry['phase'] + '.plan.json')
        plan = load(path)
        need(sha(path) == entry['plan_sha256'], 'testing.B plan hash mismatch')
        phase = next(p for p in protocol['phases'] if p['id'] == entry['phase'])
        for job in plan['jobs']:
            need('-test.count=' + str(phase['repeats_per_arm_process']) in job['command'] and
                 '-test.benchtime=' + phase['benchtime'] in job['command'], 'testing.B count/budget differs')
        if mode == 'testingb-diagnostic':
            need(plan['cases'][0]['key'] == 'skip3-rows10-forward' and len(plan['orders']) == 2,
                 'wrong testing.B diagnostic case/blocks')
        plan_hashes[str(path)] = entry['plan_sha256']
    return plan_hashes


def seal(args):
    root = safe(args.root)
    mode = args.mode
    diagnostic = mode.endswith('diagnostic')
    client = mode.startswith('client')
    live_path, live_orders = root / 'protocol.json', root / 'orders.json'
    original = load(live_path)
    source_path = root / 'provenance/source-receipt.json'
    source = load(source_path)
    check_revisions(original, source)
    compat = harness_compatibility(root, original, source)
    source_paths = artifact_pins(root, source, compat)
    harness_archive = Path(compat['client_harness_archive']) if compat is not None else root / 'frozen/overlays/client-harness.tar.gz'
    tools = root / 'tooling/tools/benchcampaign/multinode'
    scripts = root / 'frozen/tools/clientbench/scripts'
    sys.path.insert(0, str(tools))
    sm = importlib.import_module('seal_multinode')
    need(Path(sm.__file__).absolute().parent == tools, 'sealer imported from wrong directory')
    layout = root / 'cluster-ssd/layout.json'
    cluster = root / 'cluster-ssd/fdb.cluster'
    runtime = {Path(args.fdbcli), Path(args.server_binary), Path(args.native_library)}
    bins = {'before': root / 'bin/client-previous-go', 'final': root / 'bin/client-master-go',
            'cgo': root / 'bin/client-master-cgo'} if client else {
            'fetch': root / 'bin/fetch-master', 'planskip': root / 'bin/planskip-master'}
    pins = source_paths | runtime | {layout, cluster, source_path, root / 'bin/placementprobe-cgo',
            Path(__file__).absolute(), Path(__file__).with_name('p0.py').absolute(), Path(sys.executable).resolve(), *bins.values()}
    for folder in (tools, tools.parent / 'singlenode', scripts):
        files = {p for p in folder.rglob('*') if p.is_file() and '__pycache__' not in p.parts and p.suffix != '.pyc'}
        need(bool(files), 'empty tooling directory: ' + str(folder))
        pins |= files
    selected = {}
    # The M1 diagnostic deliberately has no P0 files or P0 completion dependency.
    if mode != 'client-diagnostic':
        p0pins, selected = p0_inputs(root, args.p0_root or root / 'provenance/p0')
        pins |= p0pins
    if diagnostic:
        workspace = root / 'diagnostic' / ('m1' if client else 'm2')
        out = workspace / 'plans'
        protocol_path, orders_path = workspace / 'protocol.json', workspace / 'orders.json'
        results = workspace / 'results' / 'client' if client else workspace / 'results'
        need(not os.path.lexists(workspace), 'diagnostic workspace exists; never overwrite: ' + str(workspace))
        protocol = diagnostic_protocol(original, mode)
    else:
        out = root / 'plans' / ('client' if client else 'testingb')
        protocol_path, orders_path, protocol = live_path, live_orders, original
        results = root / 'results' / 'client' if client else root / 'results'
        need(not os.path.lexists(out), 'sealed output exists; never overwrite: ' + str(out))
        phases = [p for p in protocol['phases'] if p['kind'] == ('client' if client else 'testingb')]
        if client:
            need({p['id'] for p in phases} == {'M1w1', 'M1w8', 'M1w32', 'M1w64'} and len(phases) == 4 and
                 all(p['blocks'] == 6 and p['arms'] == ['before-Go', 'final-Go', 'CGo-7.3.77'] and
                     len(p['workloads']) == 7 for p in phases), 'live client contract is not 4x7 campaigns / 6 blocks / 3 arms')
        else:
            need({p['id'] for p in phases} == {'M2rl', 'M2skip'} and len(phases) == 2 and
                 all(p['blocks'] == 6 and p['repeats_per_arm_process'] == 3 and p['benchtime'] == '3s' and
                     p['arms'] == ['final-ordinary', 'final-remote'] for p in phases), 'live M2 contract is not 6 blocks / 3 repeats / 3s')
    result_paths = {c['id']: str(results / c['id']) for c in protocol['phases']
                    if c['kind'] == ('client' if client else 'testingb')}
    for path in result_paths.values():
        need(not os.path.lexists(path), 'result output exists; refuse sealing over a prior run: ' + path)
    safe(out)
    pins |= {live_path, live_orders}
    input_hashes = {str(p.resolve()): sha(p) for p in pins}
    need(load(live_orders) == sm.make_orders(original), 'live orders differ from original protocol')
    for p in pins:
        need(p.is_file(), 'not a regular sealing input: ' + str(p))
    calls = []

    def invoke(argv):
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            rc = sm.main([str(x) for x in argv])
        calls.append(dict(argv=[str(x) for x in argv], returncode=rc, stdout=stdout.getvalue()))
        need(rc == 0, 'sealer command failed: ' + ' '.join(str(x) for x in argv))

    if diagnostic:
        workspace.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        mkdir(workspace)
        dump(protocol_path, protocol)
        invoke(['orders', '--protocol', protocol_path, '--output', orders_path])
        need(load(protocol_path)['production_revisions'] == source['production_revisions'], 'diagnostic source object changed')
    else:
        out.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    invoke(['check-orders', '--protocol', protocol_path, '--orders', orders_path])
    argv = ['client-plans' if client else 'testingb-plans', '--protocol', protocol_path, '--orders', orders_path,
            '--cluster-file', cluster, '--output-dir', out]
    for key, path in bins.items():
        argv += ['--binary', f'{key}={path}']
    for path in sorted(pins):
        argv += ['--pin', path]
    if client:
        argv += ['--scripts-dir', scripts, '--native-library', args.native_library, '--server-binary', args.server_binary,
                 '--harness-archive', harness_archive, '--results-dir', results,
                 '--adapter', tools / 'client_campaign_mn.py', '--layout', layout, '--source-receipt', source_path,
                 '--vpc-cidr', original['topology']['network']['ip_range']]
    else:
        for store, path in sorted(selected.items()):
            argv += ['--placement-receipt', store + '=' + path]
    invoke(argv)
    manifest_path = out / 'manifest.json'
    manifest = load(manifest_path)
    plan_hashes = validate_manifest(mode, manifest, out, protocol)
    required = {str(p.resolve()) for p in pins} | {str(protocol_path.resolve()), str(orders_path.resolve())}
    need(required <= set(manifest['pinned_files']), 'manifest omits required exact inputs')
    for path, digest in manifest['pinned_files'].items():
        need(sha(path) == digest, 'sealed input changed: ' + path)
    for path, digest in input_hashes.items():
        need(sha(path) == digest, 'live input changed while sealing: ' + path)
    receipt = dict(ok=True, mode=mode, workload_execution=False, diagnostic_not_campaign_results=diagnostic,
        manifest=str(manifest_path), manifest_sha256=sha(manifest_path),
        protocol=str(protocol_path), protocol_sha256=sha(protocol_path), orders=str(orders_path), orders_sha256=sha(orders_path),
        live_protocol_sha256=sha(live_path), production_revisions=source['production_revisions'],
        source_artifacts_sha256=source['source_artifacts_sha256'], harness_compatibility=compat,
        client_harness_archive=str(harness_archive), plans_sha256=plan_hashes, results=result_paths,
        campaigns=len(manifest.get('campaigns', [])), phases=manifest.get('phases', []),
        unsealed=manifest['unsealed'], pinned_files_count=len(manifest['pinned_files']), sealer_calls=calls)
    dump(out / 'sealed.json', receipt)
    print(json.dumps({k: receipt[k] for k in ('ok', 'mode', 'manifest', 'manifest_sha256', 'protocol', 'orders', 'results')}, sort_keys=True))
    return receipt


def main(argv=None):
    os.umask(0o077)
    sys.dont_write_bytecode = True
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('mode', choices=MODES)
    ap.add_argument('--root', default='/srv/fdb-bench')
    ap.add_argument('--p0-root', help='default ROOT/provenance/p0; unused by client-diagnostic')
    ap.add_argument('--fdbcli', default='/usr/bin/fdbcli')
    ap.add_argument('--server-binary', default='/usr/sbin/fdbserver')
    ap.add_argument('--native-library', default='/usr/lib/libfdb_c.so')
    args = ap.parse_args(argv)
    try:
        need(Path(args.root).is_absolute(), 'root must be absolute')
        for path in (args.fdbcli, args.server_binary, args.native_library):
            need(Path(path).is_absolute(), 'runtime paths must be absolute')
        seal(args)
        return 0
    except (Failure, OSError, ValueError, KeyError, ImportError, StopIteration) as exc:
        print('REFUSED/FAILED: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
