#!/usr/bin/env python3
"""Tests for render_dashboard.py on synthetic analysis outputs. No network."""
import csv
import json
from pathlib import Path
import tempfile
import unittest

import render_dashboard as dash

TESTDATA = Path(__file__).resolve().parent / 'testdata'


def write_client_case(root, phase, workload, scale):
    folder = root / 'analysis/client' / phase / workload
    (folder / 'cgo').mkdir(parents=True)
    with open(folder / 'cgo/absolute.csv', 'w', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=['arm', 'mean_ns', 'p50_ns', 'p99_ns', 'shutdown_process_cpu_ns_per_op'])
        writer.writeheader()
        for arm, factor in (('final', 1), ('cgo', 2), ('before', 3)):
            writer.writerow(dict(arm=arm, mean_ns=1000 * factor * scale, p50_ns=900 * factor * scale,
                                 p99_ns=5000 * factor * scale, shutdown_process_cpu_ns_per_op=700 * factor * scale))
    ratio = dict(ratio=0.5, ci95_low=0.4, ci95_high=0.6)
    metrics = ('mean_ns', 'p99_ns', 'shutdown_process_cpu_ns_per_op', 'go_alloc_bytes_per_op', 'go_mallocs_per_op')
    summary = dict(denominators={d: {m: ratio for m in metrics} for d in ('cgo', 'before')})
    (folder / 'summary.json').write_text(json.dumps(summary))


class RenderDashboardTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.protocol = json.loads((TESTDATA / 'protocol.json').read_text())
        dash.D = self.root
        dash.OUT_DIR = self.root / 'dashboard'
        dash.OUT_DIR.mkdir()

    def test_text_comes_from_protocol_and_flags(self):
        with self.assertRaisesRegex(ValueError, '--vcpus'):
            dash.describe(self.protocol)
        text = dash.describe(self.protocol, 4, 'test VM')
        self.assertEqual((text['blocks'], text['runs'], text['iterations'], text['tx_per_arm']), (6, 18, '3,000', '18,000'))
        self.assertEqual(text['measured_tx'], '594,000')
        self.assertEqual(text['final'], self.protocol['production_revisions']['final'][:8])
        cloud = dict(self.protocol, cloud=dict(type='cx-test', dedicated_vcpus=16, memory_gib=64))
        self.assertEqual(dash.describe(cloud)['machine'], 'Dedicated hcloud cx-test (16 vCPU AMD EPYC Milan, 64 GiB)')

    def test_renders_client_and_generic_phases(self):
        write_client_case(self.root, 'A', 'grv', 1)
        write_client_case(self.root, 'A', 'get100', 2)
        (self.root / 'analysis/B').mkdir(parents=True)
        (self.root / 'analysis/B/report.json').write_text(json.dumps(dict(question='Q-B', comparisons=[])))
        dash.build_html(self.protocol, 4, 'test VM')
        html = (dash.OUT_DIR / 'index.html').read_text()
        self.assertIn('FoundationDB 4-vCPU Rebenchmark', html)
        self.assertIn('test VM · FDB 7.3.77 · Go 1.26.9', html)
        payload = json.loads(html.split('const DATA = ', 1)[1].split(';\n', 1)[0])
        self.assertEqual([r['workload'] for r in payload['A']], ['grv', 'get100'])
        self.assertEqual(payload['A'][1]['cgo_mean_us'], 4.0)
        self.assertEqual(payload['A2'], [])
        self.assertEqual(list(payload['generic']), ['B'])


if __name__ == '__main__':
    unittest.main()
