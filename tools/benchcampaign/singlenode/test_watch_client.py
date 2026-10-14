#!/usr/bin/env python3
"""Tests for watch_client.py's raw-case gates on synthetic campaign folders."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import watch_client as watch

TESTDATA = Path(__file__).resolve().parent / 'testdata'
REMOTE = '/srv/fdb-bench-test'


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


class WatchClientTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        scripts = self.root / 'scripts'
        scripts.mkdir()
        for name in ('analyze.py', 'run.py'):
            (scripts / name).write_text('stand-in ' + name)
        self.code = {name: digest(scripts / name) for name in ('analyze.py', 'run.py')}
        watch.configure(scripts, REMOTE + '/', self.code['analyze.py'], self.code['run.py'])
        self.protocol = json.loads((TESTDATA / 'protocol.json').read_text())
        self.protocol_sha = digest(TESTDATA / 'protocol.json')
        self.phase = next(p for p in self.protocol['phases'] if p['id'] == 'A')
        self.pins = {REMOTE + '/tools/clientbench/scripts/' + n: d for n, d in self.code.items()}
        self.pins[REMOTE + '/protocol.json'] = self.protocol_sha
        self.case = self.root / 'grv'
        self.case.mkdir()
        plan = dict(expected_sha256=self.pins, workloads=[dict(name='grv', workers=self.phase['workers'],
                                                               iterations=self.phase['iterations'], warmup=self.phase['warmup'])])
        (self.case / 'plan.json').write_text(json.dumps(plan))

    def gate(self, state):
        return watch.raw_gate(self.case, state, 'grv', self.protocol, self.protocol_sha)

    def test_code_guard_uses_configured_hashes_and_remote_pins(self):
        self.assertEqual(watch.code_guard(self.pins), self.code)
        with self.assertRaises(ValueError):
            watch.code_guard({k.replace(REMOTE, '/elsewhere'): v for k, v in self.pins.items()})
        watch.configure(watch.SCRIPTS, REMOTE, '0' * 64, self.code['run.py'])
        with self.assertRaises(ValueError):
            watch.code_guard(self.pins)

    def test_pending_adverse_and_population_gates(self):
        self.assertIsNone(watch.raw_gate(self.root / 'missing', {}, 'grv', self.protocol, self.protocol_sha))
        self.assertIsNone(self.gate(dict(phase='A', cases={'grv': dict(status='started')})))
        (self.case / 'ADAPTER_ADVERSE.json').write_text('{}')
        self.assertEqual(self.gate(dict(phase='A', cases={'grv': dict(status='started')}))['status'], 'N/A')
        (self.case / 'ADAPTER_ADVERSE.json').unlink()
        state = dict(phase='A', cases={'grv': dict(status='complete')})
        runs = self.phase['blocks'] * len(self.phase['arms'])
        (self.case / 'schedule.json').write_text(json.dumps([dict(run_id=f'r{i}') for i in range(runs - 1)]))
        (self.case / 'complete.json').write_text(json.dumps(dict(completed=runs, scheduled=runs, binaries=self.pins)))
        with self.assertRaisesRegex(ValueError, 'incomplete'):
            self.gate(state)
        (self.case / 'schedule.json').write_text(json.dumps([dict(run_id=f'r{i}') for i in range(runs)]))
        (self.case / 'adapter-pins.finally.json').write_text(json.dumps(self.pins))
        adapter = dict(plan_sha256=digest(self.case / 'plan.json'), harness_revision='f' * 40)
        (self.case / 'adapter.json').write_text(json.dumps(adapter))
        with self.assertRaisesRegex(ValueError, 'harness identity'):
            self.gate(state)
        adapter['harness_revision'] = self.protocol['harness_revision']
        (self.case / 'adapter.json').write_text(json.dumps(adapter))
        self.assertIsNone(self.gate(state))  # Run folders still in transport.


if __name__ == '__main__':
    unittest.main()
