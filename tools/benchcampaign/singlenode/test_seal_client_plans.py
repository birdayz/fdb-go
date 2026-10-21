#!/usr/bin/env python3
"""Tests for seal_client_plans.py against the testdata protocol/orders and the
harness scripts, with stand-in files for the VM's binaries and receipts.

Run: python3 -B test_seal_client_plans.py. The harness scripts default to the
Bazel runfiles copy of tools/clientbench/scripts, else this checkout's; set
SCRIPTS_DIR to test against another frozen copy.
"""
import argparse
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

HERE = Path(__file__).resolve().parent
TESTDATA = HERE / 'testdata'


def scripts_dir():
    if os.environ.get('SCRIPTS_DIR'):
        return Path(os.environ['SCRIPTS_DIR'])
    if os.environ.get('TEST_SRCDIR') and os.environ.get('TEST_WORKSPACE'):
        return Path(os.environ['TEST_SRCDIR']) / os.environ['TEST_WORKSPACE'] / 'tools/clientbench/scripts'
    return HERE.parents[2] / 'tools/clientbench/scripts'


SCRIPTS = scripts_dir()
spec = importlib.util.spec_from_file_location('seal', HERE / 'seal_client_plans.py')
seal = importlib.util.module_from_spec(spec)
spec.loader.exec_module(seal)
HARNESS_REVISION = os.environ.get('HARNESS_REVISION',
                                  json.loads((TESTDATA / 'protocol.json').read_text())['harness_revision'])


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class SealClientPlansTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        files = {}
        for name in ['client-before-go', 'client-final-go', 'client-final-cgo', 'libfdb_c.so', 'fdbserver',
                     'fdb.cluster', 'client-harness.tar.gz', 'source-receipt.json', 'runtime-ssd.json',
                     'sources/legacy.tar.gz', 'sources/final.tar.gz']:
            path = self.root / 'vm' / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('stand-in ' + name)
            files[name] = path
        self.protocol = self.root / 'protocol.json'
        self.orders = self.root / 'orders.json'
        protocol = json.loads((TESTDATA / 'protocol.json').read_text())
        # Match the current plan generator without rewriting historical protocol data.
        protocol['runtime']['go'] = '1.27.3'
        # Protocol v4: the client campaigns' wall cap is 360 seconds, processes 180.
        protocol['harness_revision'] = HARNESS_REVISION
        for phase in protocol['phases']:
            if phase['id'] in ('A', 'A2'):
                phase['per_workload_campaign_wall_cap_seconds'] = 360
                phase['per_process_timeout_seconds'] = 180
        self.protocol.write_text(json.dumps(protocol))
        self.orders.write_text((TESTDATA / 'orders.json').read_text())
        self.files = files
        self.args = argparse.Namespace(
            scripts_dir=str(SCRIPTS), protocol=str(self.protocol), orders=str(self.orders),
            harness_archive=str(files['client-harness.tar.gz']), harness_revision=HARNESS_REVISION,
            native_library=str(files['libfdb_c.so']), server_binary=str(files['fdbserver']),
            cluster_file=str(files['fdb.cluster']), before_binary=str(files['client-before-go']),
            final_binary=str(files['client-final-go']), cgo_binary=str(files['client-final-cgo']),
            pin=[str(files['source-receipt.json']), str(files['runtime-ssd.json'])],
            pin_glob=[str(self.root / 'vm' / 'sources' / '*.tar.gz')], netns='fdb-bench-client',
            phase=['A', 'A2'], output_dir=str(self.root / 'plans' / 'client'), results_dir=str(self.root / 'results' / 'client'),
            run_template=seal.DEFAULT_RUN_TEMPLATE)

    def test_seals_every_client_campaign_with_the_frozen_orders(self):
        manifest = seal.seal(copy.copy(self.args))
        protocol = json.loads(self.protocol.read_text())
        orders = json.loads(self.orders.read_text())
        phases = {p['id']: p for p in protocol['phases']}
        self.assertEqual(len(manifest['campaigns']), len(phases['A']['workloads']) + len(phases['A2']['workloads']))
        self.assertEqual(len({c['campaign'] for c in manifest['campaigns']}), len(manifest['campaigns']))
        for pinned in [self.protocol, self.orders, self.files['client-harness.tar.gz'], self.files['runtime-ssd.json'],
                       self.files['sources/legacy.tar.gz']]:
            self.assertEqual(manifest['pinned_files'][str(pinned.resolve())], digest(pinned))
        for c in manifest['campaigns']:
            phase = phases[c['phase']]
            plan = json.loads(Path(c['plan']).read_text())
            schedule = json.loads(Path(c['schedule']).read_text())
            self.assertEqual(c['plan_sha256'], digest(c['plan']))
            self.assertEqual(plan['seed'], phase['order_seed'])
            self.assertEqual((plan['campaign_wall_cap_seconds'], plan['timeout_seconds']), (360, 180))
            self.assertEqual([a['name'] for a in plan['arms']], ['before', 'final', 'cgo'])
            self.assertEqual(plan['workloads'], [dict(name=c['workload'], iterations=phase['iterations'],
                                                     warmup=phase['warmup'], workers=phase['workers'])])
            self.assertEqual(plan['harness_id'], digest(self.files['client-harness.tar.gz']))
            self.assertEqual(plan['native_library_sha256'], digest(self.files['libfdb_c.so']))
            for arm, binary, revision in [('before', 'client-before-go', protocol['production_revisions']['before_allocation_work_after_read_parity']),
                                          ('final', 'client-final-go', protocol['production_revisions']['final']),
                                          ('cgo', 'client-final-cgo', protocol['production_revisions']['final'])]:
                spec_arm = next(a for a in plan['arms'] if a['name'] == arm)
                self.assertEqual(spec_arm['binary'], str(self.files[binary].resolve()))
                self.assertEqual(spec_arm['source_revision'], revision)
                self.assertEqual(plan['expected_sha256'][spec_arm['binary']], digest(self.files[binary]))
            self.assertEqual(len(schedule), 3 * phase['blocks'])
            for block, frozen in enumerate(orders[phase['frozen_order_key']]['orders']):
                runs = sorted((r for r in schedule if r['block'] == block), key=lambda r: r['position'])
                names = {'before': 'before-Go', 'final': 'final-Go', 'cgo': 'CGo-7.3.77'}
                self.assertEqual([names[r['arm']] for r in runs], frozen)
                for r in runs:
                    command = r['command']
                    self.assertEqual(command[command.index('--workers') + 1], str(phase['workers']))
                    self.assertEqual(command[:3], ['taskset', '--cpu-list', '8-15'])
            self.assertEqual(c['run'][3:7], ['380s', 'ip', 'netns', 'exec'])
            self.assertEqual(c['run'][-1], str(self.root / 'results' / 'client' / c['phase'] / c['workload']))
            self.assertEqual(c['campaign'], 'mx-' + seal.campaign_id(protocol['campaign'], c['phase'], c['workload']))

    def test_adapter_run_template(self):
        args = copy.copy(self.args)
        adapter = self.root / 'vm' / 'client_campaign.py'
        adapter.write_text('stand-in adapter')
        args.pin = [*args.pin, str(adapter)]
        args.phase = ['A2']
        args.run_template = ['{python}', '-B', str(adapter), 'run', '--plan', '{plan}', '--output', '{output}',
                             '--phase', '{phase}', '--workload', '{workload}', '--lifetime-deadline', '2026-10-15T00:19:00Z']
        manifest = seal.seal(args)
        self.assertEqual(manifest['pinned_files'][str(adapter.resolve())], digest(adapter))
        for c in manifest['campaigns']:
            self.assertEqual(c['run'][2:4], [str(adapter), 'run'])
            self.assertEqual(c['run'][c['run'].index('--plan') + 1], c['plan'])
            self.assertEqual(c['run'][c['run'].index('--workload') + 1], c['workload'])
        args = copy.copy(self.args)
        args.output_dir = str(self.root / 'plans' / 'other')
        args.run_template = ['{python}', str(adapter)]
        with self.assertRaisesRegex(ValueError, 'plan'):
            seal.seal(args)

    def test_run_template_seals_root_and_server_address(self):
        args = copy.copy(self.args)
        args.phase = ['A2']
        args.run_template = ['env', 'FDB_BENCH_ROOT={root}', 'FDB_BENCH_SERVER_ADDR={server_addr}', '{python}', '-B',
                             '{root}/client_campaign.py', 'run', '--plan', '{plan}', '--output', '{output}']
        with mock.patch.dict(os.environ, {}, clear=True), self.assertRaisesRegex(ValueError, 'FDB_BENCH_SERVER_ADDR'):
            seal.seal(copy.copy(args))
        args.output_dir = str(self.root / 'plans' / 'with-server')
        with mock.patch.dict(os.environ, FDB_BENCH_SERVER_ADDR='192.0.2.1:4500'):
            manifest = seal.seal(args)
        for c in manifest['campaigns']:
            self.assertEqual(c['run'][1:3], ['FDB_BENCH_ROOT=' + str(seal.ROOT), 'FDB_BENCH_SERVER_ADDR=192.0.2.1:4500'])
            self.assertEqual(c['run'][5], str(seal.ROOT) + '/client_campaign.py')

    def test_rejects_a_schedule_that_differs_from_the_frozen_orders(self):
        orders = json.loads(self.orders.read_text())
        orders['A']['orders'][0], orders['A']['orders'][1] = orders['A']['orders'][1], orders['A']['orders'][0]
        self.orders.write_text(json.dumps(orders))
        with self.assertRaisesRegex(ValueError, 'frozen order'):
            seal.seal(copy.copy(self.args))

    def test_rejects_a_protocol_with_the_old_toolchain(self):
        protocol = json.loads(self.protocol.read_text())
        protocol['runtime']['go'] = '1.26.9'
        self.protocol.write_text(json.dumps(protocol))
        with self.assertRaisesRegex(ValueError, 'plan API/Go version differs'):
            seal.seal(copy.copy(self.args))

    def test_rejects_other_harness_revision_missing_pins_and_reused_output(self):
        args = copy.copy(self.args)
        args.harness_revision = '0' * 40
        with self.assertRaisesRegex(ValueError, 'harness_revision'):
            seal.seal(args)
        args = copy.copy(self.args)
        args.pin_glob = [str(self.root / 'vm' / 'missing' / '*')]
        with self.assertRaisesRegex(ValueError, 'matched nothing'):
            seal.seal(args)
        (self.root / 'plans' / 'client').mkdir(parents=True, exist_ok=True)
        with self.assertRaises(FileExistsError):
            seal.seal(copy.copy(self.args))


if __name__ == '__main__':
    unittest.main()
