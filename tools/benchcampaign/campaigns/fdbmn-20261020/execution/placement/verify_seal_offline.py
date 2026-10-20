#!/usr/bin/env python3
"""Exercise the real sealer in all four modes on private fake files. No commands run."""
import argparse
import ast
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import types
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).absolute().parent
sys.path.insert(0, str(HERE))
import seal_live as sl
from p0 import Failure, dump, load, need, sha


def main():
    os.umask(0o077)
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--campaign', default=str(HERE.parent.parent))
    ap.add_argument('--archive', default='${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014')
    ap.add_argument('--compat', action='store_true', help='exercise declared 9-artifact cleanup compatibility fixture')
    args = ap.parse_args()
    c, a = Path(args.campaign), Path(args.archive)
    ast.parse((HERE / 'seal_live.py').read_text())
    with tempfile.TemporaryDirectory(dir=HERE) as tmp, patch.object(subprocess, 'run', side_effect=AssertionError('no command execution')):
        root = Path(tmp)
        for folder in ('tooling/tools/benchcampaign/multinode', 'tooling/tools/benchcampaign/singlenode',
                       'frozen/tools/clientbench/scripts'):
            shutil.copytree(c / folder, root / folder)
        for folder in ('provenance', 'sources', 'frozen/overlays', 'cluster-ssd', 'bin', 'runtime'):
            (root / folder).mkdir(parents=True, exist_ok=True)
        proto = load(c / 'protocol.json')
        proto.pop('harness_compatibility', None)
        if args.compat:
            (root / 'frozen/cleanup-compat').mkdir()
            for name in ('client-harness.tar.gz', 'combined.patch'):
                (root / 'frozen/cleanup-compat' / name).write_bytes(('fake hash-only compat ' + name).encode())
            proto['harness_compatibility'] = {
                'description': 'Declared cleanup lifecycle + matching validator compatibility; test fixture only',
                'upstream_commit': sl.CLEANUP_COMMIT,
                'client_harness_archive': str(root / 'frozen/cleanup-compat/client-harness.tar.gz'),
                'patch_file': str(root / 'frozen/cleanup-compat/combined.patch'),
                'archive_sha256': sha(root / 'frozen/cleanup-compat/client-harness.tar.gz'),
                'patch_sha256': sha(root / 'frozen/cleanup-compat/combined.patch')}
        dump(root / 'protocol.json', proto)
        sys.path.insert(0, str(root / 'tooling/tools/benchcampaign/multinode'))
        import seal_multinode as sm
        dump(root / 'orders.json', sm.make_orders(proto))
        source = {'harness_commit': sl.HARNESS, 'production_revisions': copy.deepcopy(proto['production_revisions']),
                  'source_artifacts_sha256': {}}
        for rel in ('sources/master.tar.gz', 'sources/previous.tar.gz',
                    *('frozen/overlays/' + n + '.tar.gz' for n in sl.OVERLAYS)):
            path = root / rel
            path.write_bytes(('fake hash-only archive ' + rel).encode())
            source['source_artifacts_sha256'][str(path)] = sha(path)
        if args.compat:
            source['harness_compatibility'] = copy.deepcopy(proto['harness_compatibility'])
            for key in ('client_harness_archive', 'patch_file'):
                path = source['harness_compatibility'][key]
                source['source_artifacts_sha256'][path] = sha(path)
            compat = sl.harness_compatibility(root, proto, source)
            need(len(sl.artifact_pins(root, source, compat)) == 9, 'declared compatibility did not require exactly 9 artifacts')
            bad_sources = []
            bad = copy.deepcopy(source)
            bad.pop('harness_compatibility')
            bad_sources.append(bad)
            for field, value in (('description', 'different'), ('upstream_commit', '0' * 40),
                                 ('client_harness_archive', '/wrong/archive'), ('archive_sha256', '0' * 64)):
                bad = copy.deepcopy(source)
                bad['harness_compatibility'][field] = value
                bad_sources.append(bad)
            for bad in bad_sources:
                try:
                    sl.harness_compatibility(root, proto, bad)
                except Failure:
                    pass
                else:
                    raise AssertionError('missing/mismatched compatibility metadata accepted')
            for field, value in (('upstream_commit', '0' * 40), ('client_harness_archive', '/wrong/archive'),
                                 ('archive_sha256', '0' * 64), ('patch_sha256', '0' * 64)):
                badproto, badsource = copy.deepcopy(proto), copy.deepcopy(source)
                badproto['harness_compatibility'][field] = value
                badsource['harness_compatibility'][field] = value
                try:
                    sl.harness_compatibility(root, badproto, badsource)
                except Failure:
                    pass
                else:
                    raise AssertionError('matching but invalid compatibility metadata accepted: ' + field)
            bad = copy.deepcopy(source)
            for key in ('client_harness_archive', 'patch_file'):
                bad['source_artifacts_sha256'].pop(compat[key])
            try:
                sl.artifact_pins(root, bad, compat)
            except Failure:
                pass
            else:
                raise AssertionError('declared compatibility accepted only 7 source artifacts')
            bad = copy.deepcopy(source)
            bad['source_artifacts_sha256'][str(root / 'extra-artifact')] = '0' * 64
            try:
                sl.artifact_pins(root, bad, compat)
            except Failure:
                pass
            else:
                raise AssertionError('declared compatibility accepted an extra source artifact')
            badproto, badsource = copy.deepcopy(proto), copy.deepcopy(source)
            badproto['harness_compatibility']['extra'] = True
            badsource['harness_compatibility']['extra'] = True
            try:
                sl.harness_compatibility(root, badproto, badsource)
            except Failure:
                pass
            else:
                raise AssertionError('extra compatibility metadata field accepted')
            baseproto = copy.deepcopy(proto)
            baseproto.pop('harness_compatibility')
            try:
                sl.harness_compatibility(root, baseproto, source)
            except Failure:
                pass
            else:
                raise AssertionError('source-only compatibility declaration accepted')
            print('PASS compatibility controls: declaration parity/equality, full upstream/path/hash checks, exact9 not7')
        dump(root / 'provenance/source-receipt.json', source)
        shutil.copyfile(a / 'cluster-ssd/fdb.cluster', root / 'cluster-ssd/fdb.cluster')
        shutil.copyfile(a / 'cluster-ssd/layout.json', root / 'cluster-ssd/layout.json')
        for name in ('client-previous-go', 'client-master-go', 'client-master-cgo', 'fetch-master', 'planskip-master', 'placementprobe-cgo'):
            (root / 'bin' / name).write_bytes(('never executable ' + name).encode())
        for name in ('fdbcli', 'fdbserver', 'libfdb_c.so'):
            (root / 'runtime' / name).write_bytes(('never executed ' + name).encode())
        options = dict(root=str(root), p0_root=None, fdbcli=str(root / 'runtime/fdbcli'),
                       server_binary=str(root / 'runtime/fdbserver'), native_library=str(root / 'runtime/libfdb_c.so'))
        original = sha(root / 'protocol.json')
        source_original = sha(root / 'provenance/source-receipt.json')
        with contextlib.redirect_stdout(io.StringIO()):
            diagnostic = sl.seal(types.SimpleNamespace(mode='client-diagnostic', **options))
        need(diagnostic['campaigns'] == 1, 'diagnostic count')
        need(not (root / 'provenance/p0').exists(), 'M1 diagnostic touched P0')
        need(diagnostic['results'] == {'M1w1': str(root / 'diagnostic/m1/results/client/M1w1')}, 'M1 results prefix')
        need(load(diagnostic['protocol'])['production_revisions'] == source['production_revisions'], 'diagnostic source changed')
        expected_archive = root / ('frozen/cleanup-compat/client-harness.tar.gz' if args.compat else 'frozen/overlays/client-harness.tar.gz')
        need(diagnostic['client_harness_archive'] == str(expected_archive), 'wrong client diagnostic archive')
        diagmanifest = load(diagnostic['manifest'])
        need(all(load(x['plan'])['harness_id'] == sha(expected_archive) for x in diagmanifest['campaigns']),
             'client diagnostic plan did not use selected harness archive')
        if args.compat:
            need(diagnostic['harness_compatibility'] == source['harness_compatibility'] ==
                 load(diagnostic['protocol'])['harness_compatibility'], 'diagnostic compatibility metadata differs')
            need(set(source['source_artifacts_sha256']) <= set(diagmanifest['pinned_files']), 'compat artifacts unpinned')
        print('PASS client-diagnostic: 1 grv campaign, 4 processes; no P0 dependency; selected archive hash in plan')
        p0 = root / 'provenance/p0'
        p0.mkdir()
        selected, fixtures = {}, {}
        for n in ('small', 'split', 'planskip'):
            fixture = p0 / ('fixture-' + n + '.json')
            receipt = p0 / ('placement-' + n + '.json')
            shutil.copyfile(a / f'provenance/p0/fixture-{n}.json', fixture)
            shutil.copyfile(a / f'provenance/p0/placement-{n}-a3.json', receipt)
            selected['benchfetch/' + n], fixtures['benchfetch/' + n] = str(receipt), str(fixture)
        dump(p0 / 'selected-receipts.json', selected)
        dump(p0 / 'fixture-receipts.json', fixtures)
        dump(p0 / 'placement-command.json', ['never execute this'])
        p0paths = [p0 / 'selected-receipts.json', p0 / 'fixture-receipts.json', p0 / 'placement-command.json',
                   *map(Path, selected.values()), *map(Path, fixtures.values()), HERE / 'p0.py', HERE / 'placement_checked.py']
        dump(p0 / 'seal-pins.json', {str(p): sha(p) for p in p0paths})
        dump(p0 / 'complete.json', dict(ok=True, integral=True, exit_status=3, dd_left_off=True,
            cluster_file_sha256=sha(root / 'cluster-ssd/fdb.cluster'), placementprobe_sha256=sha(root / 'bin/placementprobe-cgo'),
            selected_receipts=selected, placement_command_sha256=sha(p0 / 'placement-command.json')))
        for mode in ('testingb-diagnostic', 'client-final', 'testingb-final'):
            with contextlib.redirect_stdout(io.StringIO()):
                receipt = sl.seal(types.SimpleNamespace(mode=mode, **options))
            manifest = load(receipt['manifest'])
            need(set(load(p0 / 'seal-pins.json')) <= set(manifest['pinned_files']), 'missing P0 exact pins')
            need(receipt['unsealed'] == [], 'unsealed work')
            if mode == 'testingb-diagnostic':
                need(receipt['phases'] == [{'phase': 'M2skip', 'cases': 1, 'jobs': 4,
                     'plan_sha256': receipt['phases'][0]['plan_sha256']}], 'M2 diagnostic count')
                need(receipt['results'] == {'M2skip': str(root / 'diagnostic/m2/results/M2skip')}, 'M2 results prefix')
            if mode == 'client-final':
                need(receipt['campaigns'] == 28, 'client final count')
                need(all(load(x['plan'])['harness_id'] == sha(expected_archive) for x in manifest['campaigns']),
                     'client final plans did not use selected harness archive')
            if args.compat:
                need(receipt['harness_compatibility'] == source['harness_compatibility'], 'sealed compatibility metadata differs')
                need(set(source['source_artifacts_sha256']) <= set(manifest['pinned_files']), 'compat artifacts unpinned')
            if mode == 'testingb-final':
                need([(p['phase'], p['cases'], p['jobs']) for p in receipt['phases']] ==
                     [('M2rl', 12, 144), ('M2skip', 4, 48)], 'M2 final count')
            need(receipt['manifest_sha256'] == sha(receipt['manifest']), 'manifest hash receipt')
            print('PASS ' + mode + ': exact requested sealed population, all P0/source/runtime/tool pins')
        for mode in sl.MODES:
            try:
                with contextlib.redirect_stdout(io.StringIO()):
                    sl.seal(types.SimpleNamespace(mode=mode, **options))
            except Failure as e:
                need('exists' in str(e), 'wrong overwrite refusal: ' + str(e))
            else:
                raise AssertionError('existing output accepted: ' + mode)
        need(sha(root / 'protocol.json') == original, 'real protocol was modified')
        need(sha(root / 'provenance/source-receipt.json') == source_original, 'source object was modified')
        print('PASS all 4 overwrite refusals; real protocol/source untouched; zero subprocesses')
    return 0


if __name__ == '__main__':
    sys.exit(main())
