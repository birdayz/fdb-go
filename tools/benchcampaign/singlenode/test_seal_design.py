#!/usr/bin/env python3
"""Tests for seal_design.py: the version-1 testdata design must reproduce the
sealed testdata orders and phase contracts exactly. No network or VM access."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
TESTDATA = HERE / 'testdata'
ENV = dict(FDB_BENCH_HOST_VETH_ADDR='192.0.2.1/30', FDB_BENCH_PEER_VETH_ADDR='192.0.2.2/30',
           FDB_BENCH_SERVER_ADDR='192.0.2.1:4500')


def run(folder, env, *extra):
    clean = {k: v for k, v in os.environ.items() if not k.startswith('FDB_BENCH_')}
    return subprocess.run([sys.executable, '-B', str(HERE / 'seal_design.py'), '--dir', str(folder), *extra],
                          env={**clean, **env}, capture_output=True, text=True)


class SealDesignTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        shutil.copy(TESTDATA / 'protocol-v1.json', self.dir / 'protocol.json')

    def test_reproduces_sealed_orders_and_phases(self):
        result = run(self.dir, ENV)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.dir / 'orders.json').read_text(), (TESTDATA / 'orders.json').read_text())
        self.assertEqual((self.dir / 'protocol-v1.json').read_text(), (TESTDATA / 'protocol-v1.json').read_text())
        sealed = json.loads((self.dir / 'protocol.json').read_text())
        frozen = json.loads((TESTDATA / 'protocol.json').read_text())
        self.assertEqual(sealed['version'], 2)
        self.assertEqual(sealed['environment_policy'], frozen['environment_policy'])
        # The frozen fixture is version 4: a later amendment raised only the client caps.
        amended = ('per_workload_campaign_wall_cap_seconds', 'wall_cap_seconds')
        for got, want in zip(sealed['phases'], frozen['phases']):
            if got['id'] in ('A', 'A2'):
                got = {k: v for k, v in got.items() if k not in amended}
                want = {k: v for k, v in want.items() if k not in amended}
            self.assertEqual(got, want)
        net = sealed['network_sensitivity']
        self.assertEqual(net['topology'], 'Host fdb-bench-host192.0.2.1/30 -> namespace fdb-bench-client/'
                                          'fdb-bench-peer192.0.2.2/30; server192.0.2.1:4500; no namespace default route.')
        self.assertEqual(net['client_to_server'], 'ip netns exec fdb-bench-client tc qdisc replace dev '
                                                  'fdb-bench-peer root netem limit100000 delay250us')

    def test_seed_base_and_names_are_parameters(self):
        env = dict(ENV, FDB_BENCH_NETNS='ns-x', FDB_BENCH_HOST_VETH='vh', FDB_BENCH_PEER_VETH='vp')
        result = run(self.dir, env, '--order-seed-base', '1000')
        self.assertEqual(result.returncode, 0, result.stderr)
        sealed = json.loads((self.dir / 'protocol.json').read_text())
        self.assertEqual([p['order_seed'] for p in sealed['phases']], [1000 + 100 * i for i in range(8)])
        self.assertEqual(sealed['network_sensitivity']['server_to_client'],
                         'tc qdisc replace dev vh root netem limit100000 delay250us')
        self.assertIn('namespace ns-x/vp192.0.2.2/30', sealed['network_sensitivity']['topology'])

    def test_requires_topology_environment(self):
        for missing in ENV:
            result = run(self.dir, {k: v for k, v in ENV.items() if k != missing})
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(missing, result.stderr)
            self.assertFalse((self.dir / 'orders.json').exists())


if __name__ == '__main__':
    unittest.main()
