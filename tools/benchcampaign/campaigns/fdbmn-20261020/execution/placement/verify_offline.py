#!/usr/bin/env python3
"""Read-only archive + fake-operation checks. Never executes a cluster command."""
import argparse
import ast
import contextlib
import copy
import io
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
HERE = Path(__file__).absolute().parent
sys.path.insert(0, str(HERE))
import p0

ap = argparse.ArgumentParser(description=__doc__)
ap.add_argument('--campaign', default=str(HERE.parent.parent))
ap.add_argument('--archive', default='${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014')
args = ap.parse_args()
C, A = Path(args.campaign), Path(args.archive)
sys.path.insert(0, str(C / 'tooling/tools/benchcampaign/multinode'))
import placement_receipt as pr


def old(name):
    return json.loads((A / 'provenance/p0' / name).read_text())


class Checks(unittest.TestCase):
    def test_syntax(self):
        for name in ('p0.py', 'placement_checked.py', 'verify_offline.py'):
            ast.parse((HERE / name).read_text(), filename=name)

    def test_exact_frozen_fixtures_and_no_leaf_regex(self):
        for name in p0.STORES:
            text = (A / f'provenance/p0/seed-{name}-a1.log').read_text()
            # Original archive had a timed leaf. Only its fixture receipt is
            # the oracle; new fixture-only execution must reject those logs.
            with self.assertRaises(p0.Failure):
                p0.fixture_from_log(text, name)
            line = next(s for s in text.splitlines() if '"phase":"fixture"' in s)
            receipt = p0.fixture_from_log(line + '\nPASS\n', name)
            self.assertEqual(receipt, old(f'fixture-{name}.json'))
            command = p0.seed_argv(Path('/does-not-execute'), name)
            regex = next(a.removeprefix('-test.bench=') for a in command if a.startswith('-test.bench='))
            self.assertTrue(regex.endswith('/^P0_FIXTURE_ONLY_NO_CELL$'))
            self.assertNotIn('method=', regex)
            with self.assertRaises(p0.Failure):
                p0.fixture_from_log(line.replace('"rows":1000', '"rows":999') + '\nPASS\n', name)
            with self.assertRaises(p0.Failure):
                p0.fixture_from_log(line + '\n' + line + '\nPASS\n', name)

    def test_integral_partial_is_not_integrity_failure(self):
        for name in p0.STORES:
            receipt = old(f'placement-{name}-a3.json')
            rc = 0 if receipt['passed'] else 3
            self.assertEqual(p0.receipt_kind(rc, receipt), receipt['passed'])
            bad = copy.deepcopy(receipt)
            bad['integrity_ok'] = False
            with self.assertRaises(p0.Failure):
                p0.receipt_kind(3, bad)
            with self.assertRaises(p0.Failure):
                p0.receipt_kind(1, receipt)
            bad = copy.deepcopy(receipt)
            bad['cells'] = {}
            with self.assertRaises(p0.Failure):
                p0.receipt_kind(3, bad)

    def test_all_original_offline_controls_revalidated_from_archive(self):
        for k in (1, 2, 3):
            for name in p0.STORES:
                fixture = A / f'provenance/p0/fixture-{name}.json'
                probe = old(f'probe-{name}-a{k}.json')
                status = old(f'status-probe-a{k}.json')
                preset = 'planskip' if name == 'planskip' else 'benchfetch'
                report = pr.run_controls(status, probe, pr.PRESETS[preset], layout='recordlayer',
                    rows=1000, record_format='split' if name == 'split' else 'unsplit',
                    expect_prefix=pr.pack(['benchfetch', name]), fixture=json.loads(fixture.read_text()),
                    replicas=2, preset=preset, fixture_sha256=p0.sha(fixture), expect_cluster=probe['cluster'])
                p0.controls_ok(report, pr)
                # An edited report cannot make a missing group vacuously pass.
                bad = copy.deepcopy(report)
                bad['provenance_mutations'] = {}
                with self.assertRaises(p0.Failure):
                    p0.controls_ok(bad, pr)

    def test_exact_on_cluster_mutation_evidence_from_archive(self):
        q = A / 'provenance/p0-qualification'
        original = json.loads((q / 'probe-orig.json').read_text())
        for defect in p0.DEFECTS:
            mutated = json.loads((q / f'probe-{defect}.json').read_text())
            receipt = json.loads((q / f'receipt-{defect}.json').read_text())
            p0.mutation_ok(original, mutated, receipt, defect, pr)
            with self.assertRaises(p0.Failure):
                p0.mutation_ok(original, original, receipt, defect, pr)

    def test_epoch_check_and_drift(self):
        receipts = [old(f'placement-{n}-a3.json') for n in p0.STORES]
        p0.same_epoch(receipts)
        receipts[2]['move_keys_owner_hex'] = 'bad'
        with self.assertRaises(p0.Failure):
            p0.same_epoch(receipts)

    def test_private_exclusive_outputs_symlink_refusal(self):
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            folder = p0.mkdir(Path(tmp) / 'private')
            output = folder / 'receipt.json'
            p0.dump(output, {'ok': True})
            self.assertEqual(folder.stat().st_mode & 0o777, 0o700)
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                p0.dump(output, {})
            link = Path(tmp) / 'link'
            link.symlink_to(folder, target_is_directory=True)
            with self.assertRaises(p0.Failure):
                p0.safe(link / 'new.json')

    def test_attempt_order_all_stores_before_single_freeze(self):
        class Fake(p0.P0):
            def __init__(self, root):
                self.out = root
                self.events = []
                self.cluster = Path('/never-executed.cluster')
                self.fetch, self.skip = Path('/never-fetch'), Path('/never-skip')
                self.cg = types.SimpleNamespace(role_identity=lambda status: {'roles': 'stable'})
                self.a = types.SimpleNamespace(settle_timeout_seconds=1200)

            def cli_exec(self, folder, label, command):
                self.events.append(command)

            def run(self, folder, label, command, **kw):
                name = folder.name
                self.events.append('seed-' + name)
                text = (A / f'provenance/p0/seed-{name}-a1.log').read_text()
                line = next(s for s in text.splitlines() if '"phase":"fixture"' in s)
                return 0, line + '\nPASS\n', ''

            def pp_cmd(self, folder, label, cmd, store, fixture=None, **flags):
                self.events.append(cmd + '-' + store.rsplit('/', 1)[-1])
                if cmd == 'ballast-write':
                    count = math.ceil(flags['ballast_mib'] * 1024 * 1024 / 100000)
                    return None, dict(keys_written=count, total_bytes=count * 100000, value_bytes=100000)
                return None, dict(empty=True, keys_found=0, read_version=7)

            def settle(self, folder):
                self.events.append('settle120')

            def status(self, folder, label, mode):
                self.events.append('status-' + mode)
                return {}, []

            def probe(self, folder, name, store, fixture):
                suffix = store.rsplit('/', 1)[-1]
                self.events.append('probe-' + suffix)
                return folder / 'fake.json', old(f'placement-{suffix}-a3.json'), {}, {'roles': 'stable'}

        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            fake = Fake(Path(tmp))
            fake.attempt(1, 256)
            expected = ['datadistribution on']
            for n in p0.STORES:
                expected += ['seed-' + n, 'ballast-write-' + n]
            expected += ['settle120', 'datadistribution off', 'status-disabled']
            for n in p0.STORES:
                expected += ['ballast-clear-' + n, 'ballast-verify-empty-' + n]
            expected += ['probe-' + n for n in p0.STORES]
            self.assertEqual(fake.events, expected)

    def test_first_all_pass_else_attempt_three_and_stop_on_integrity(self):
        class Select(p0.P0):
            def __init__(self, root, passed, fail=None):
                self.out, self.pins = root, {}
                self.calls, self.passed, self.fail = [], passed, fail

            def qualify(self):
                self.calls.append('qualify')

            def attempt(self, k, mib):
                self.calls.append((k, mib))
                if k == self.fail:
                    raise p0.Failure('integrity defect; not retryable')
                return self.passed[k - 1], None, None, None, None

            def finish(self, k, *_):
                self.calls.append(('selected', k))
                return 0 if self.passed[k - 1] else 3

        for passed, selected in (((True, True, True), 1), ((False, True, True), 2), ((False, False, False), 3)):
            with tempfile.TemporaryDirectory(dir=HERE) as tmp:
                runner = Select(Path(tmp) / 'new', passed)
                self.assertEqual(runner.execute(), 0 if passed[selected - 1] else 3)
                self.assertEqual(runner.calls[-1], ('selected', selected))
                self.assertEqual(runner.calls[1:-1], list(enumerate((256, 512, 1024)[:selected], 1)))
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            runner = Select(Path(tmp) / 'new', (False, False, False), fail=1)
            with self.assertRaises(p0.Failure):
                runner.execute()
            self.assertEqual(runner.calls, ['qualify', (1, 256)])

    def test_final_sweep_compares_all_stores_and_emits_exact_seal_pins(self):
        class Final(p0.P0):
            def __init__(self, root, drift=False):
                self.out = root
                self.a = types.SimpleNamespace(expect_cluster_sha256='a' * 64, expect_placementprobe_sha256='b' * 64)
                self.pp, self.cli = Path('/never-pp'), Path('/never-cli')
                self.tools = C / 'tooling/tools/benchcampaign/multinode'
                self.cg = types.SimpleNamespace(role_identity=lambda _: 'stable')
                self.pr, self.pins, self.events, self.drift = pr, {}, [], drift

            def probe(self, folder, name, store, fixture):
                self.events.append(store)
                suffix = store.rsplit('/', 1)[-1]
                receipt = old(f'placement-{suffix}-a3.json')
                if self.drift:
                    receipt['key_servers_sha256'] = 'changed'
                path = folder / 'final-receipt.json'
                p0.dump(path, receipt)
                return path, receipt, {}, 'stable'

            def status(self, folder, label, mode):
                self.events.append('final-status-' + mode)
                return {}, []

            def check_pins(self):
                pass

        fixtures = {n: A / f'provenance/p0/fixture-{n}.json' for n in p0.STORES}
        receipts = {n: (None, old(f'placement-{n}-a3.json')) for n in p0.STORES}
        for drift in (False, True):
            with tempfile.TemporaryDirectory(dir=HERE) as tmp:
                runner = Final(Path(tmp), drift)
                if drift:
                    with self.assertRaises(p0.Failure):
                        runner.finish(3, None, fixtures, receipts, 'stable')
                    self.assertFalse((Path(tmp) / 'complete.json').exists())
                    continue
                expected = 0 if all(r['passed'] for _, r in receipts.values()) else 3
                self.assertEqual(runner.finish(3, None, fixtures, receipts, 'stable'), expected)
                self.assertEqual(runner.events, ['benchfetch/' + n for n in p0.STORES] + ['final-status-disabled'])
                mapping = p0.load(Path(tmp) / 'fixture-receipts.json')
                self.assertEqual(mapping, {'benchfetch/' + n: str(f) for n, f in fixtures.items()})
                pins = p0.load(Path(tmp) / 'seal-pins.json')
                for path, digest in pins.items():
                    self.assertEqual(p0.sha(path), digest)
                self.assertTrue(set(mapping.values()) <= set(pins))
                command = p0.load(Path(tmp) / 'placement-command.json')
                self.assertEqual(command[command.index('--expect-placementprobe-sha256') + 1], 'b' * 64)
                self.assertEqual(command[command.index('--expect-cluster-sha256') + 1], 'a' * 64)
                self.assertTrue(p0.load(Path(tmp) / 'complete.json')['dd_left_off'])

    def test_r2_wrapper_preserves_frozen_command_and_literal_hash_receipts(self):
        import placement_checked as checked
        import placement_command as pc
        with tempfile.TemporaryDirectory(dir=HERE) as tmp:
            root = Path(tmp)
            cluster = root / 'fdb.cluster'
            cluster.write_bytes((A / 'cluster-ssd/fdb.cluster').read_bytes())
            pp = root / 'fake-pp-not-executable'
            pp.write_bytes(b'never execute this')
            fixture = A / 'provenance/p0/fixture-small.json'
            fixtures = root / 'fixtures.json'
            p0.dump(fixtures, {'benchfetch/small': str(fixture)})
            output = root / 'receipt.json'
            command = ['--expect-placementprobe-sha256', p0.sha(pp), '--tooling', str(C / 'tooling/tools/benchcampaign/multinode'),
                '--store', 'benchfetch/small', '--case', 'small-rows10-forward', '--when', 'before', '--preset', 'benchfetch',
                '--layout', 'recordlayer', '--record-format', 'unsplit', '--cluster-file', str(cluster),
                '--expect-cluster-sha256', p0.sha(cluster), '--output', str(output), '--placementprobe', str(pp),
                '--fdbcli', '/never-execute-fdbcli', '--fixture-receipts', str(fixtures), '--work-dir', str(root / 'work')]

            def fake(argv, timeout=None):
                if argv[1] == 'probe':
                    p0.dump(Path(argv[argv.index('--output') + 1]), old('probe-small-a3.json'))
                    return 0, 'fake probe\n', ''
                if argv[-2:] == ['--exec', 'status json']:
                    return 0, json.dumps(old('status-probe-a3.json')), ''
                raise AssertionError(argv)

            with patch.object(pc.mc, 'default_runner', side_effect=fake) as mock, contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(checked.main(command), 0)
                self.assertEqual(mock.call_count, 2)
            receipt = p0.load(str(output) + '.command.json')
            self.assertEqual(receipt['placementprobe_sha256'], p0.sha(pp))
            self.assertEqual(receipt['cluster_file_sha256'], p0.sha(cluster))
            self.assertEqual(len(receipt['commands']), 2)
            for r in receipt['commands']:
                self.assertEqual(r['placementprobe_sha256'], p0.sha(pp))
                self.assertEqual(r['cluster_file_sha256'], p0.sha(cluster))
            # New output path, wrong PP bytes: refuse without executing either command.
            output2 = root / 'rejected.json'
            command[command.index('--output') + 1] = str(output2)
            command[command.index('--case') + 1] = 'different-case'
            pp.write_bytes(b'changed bytes')
            with patch.object(pc.mc, 'default_runner', side_effect=AssertionError('must not run')), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(checked.main(command), 1)
            self.assertFalse(output2.exists())
            self.assertIn('pinned placementprobe changed', p0.load(str(output2) + '.command.json')['error'])
            pp.write_bytes(b'never execute this')
            for timed_step in ('probe', 'status'):
                command[command.index('--output') + 1] = str(root / (timed_step + '-timeout.json'))
                command[command.index('--case') + 1] = timed_step + '-timeout'

                def timeout_fake(argv, timeout=None):
                    is_probe = argv[1] == 'probe'
                    if is_probe == (timed_step == 'probe'):
                        raise subprocess.TimeoutExpired(argv, timeout, output=b'partial-out', stderr=b'partial-err')
                    return fake(argv, timeout)

                with patch.object(pc.mc, 'default_runner', side_effect=timeout_fake), contextlib.redirect_stderr(io.StringIO()):
                    self.assertEqual(checked.main(command), 1)
                failure = p0.load(str(root / (timed_step + '-timeout.json')) + '.command.json')
                self.assertTrue(failure['commands'][-1]['timeout'])
                self.assertEqual(failure['commands'][-1]['placementprobe_sha256'], p0.sha(pp))
                self.assertEqual(failure['commands'][-1]['cluster_file_sha256'], p0.sha(cluster))
                self.assertIsNone(failure['commands'][-1]['returncode'])
                self.assertEqual(failure['commands'][-1]['stdout'], 'partial-out')
                self.assertEqual(failure['commands'][-1]['stderr'], 'partial-err')
                self.assertEqual(len(failure['commands']), 1 if timed_step == 'probe' else 2)
                self.assertIn('timed out', failure['error'])

    def test_no_execute_refuses_before_any_cluster_command(self):
        with patch('subprocess.run', side_effect=AssertionError('must not run commands')):
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(p0.main(['--output', '/does-not-exist', '--expect-cluster-sha256', '0' * 64,
                    '--expect-placementprobe-sha256', '0' * 64, '--expect-fetch-sha256', '0' * 64,
                    '--expect-planskip-sha256', '0' * 64]), 2)


if __name__ == '__main__':
    os.umask(0o077)
    unittest.main(argv=[sys.argv[0]], verbosity=2)
