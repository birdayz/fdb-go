#!/usr/bin/env python3
import argparse
import collections
import copy
from pathlib import Path
import tempfile
import unittest

import matrix_plan
import r3_plan
import run


class MatrixPlanTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ['before', 'after', 'cgo', 'cluster', 'native', 'server', 'harness']:
            (self.root / name).write_text(name)
        self.args = argparse.Namespace(
            arm=[f'g9a={self.root / "before"}@{"a" * 40}', f'g267={self.root / "after"}@{"b" * 40}',
                 f'cgo={self.root / "cgo"}@{"b" * 40}'],
            workload=['read1000'], iterations=500, warmup=50, blocks=12,
            cluster_file=self.root / 'cluster', native_library=self.root / 'native', server_binary=self.root / 'server',
            harness_id=r3_plan.digest(self.root / 'harness'), pin=[self.root / 'harness'], campaign_id='c' * 29,
            seed=641001, gomaxprocs=8, housekeeping_cpus='0,8', server_cpus='1-3,9-11', client_cpus='4-7,12-15')

    def test_three_arm_plan_is_counterbalanced(self):
        plan = matrix_plan.make_plan(self.args)
        schedule = run.make_schedule(plan, self.root / 'out')
        self.assertEqual(len(schedule), 12 * 1 * 3)
        self.assertEqual(schedule, run.make_schedule(plan, self.root / 'out'))
        for workload in self.args.workload:
            orders = collections.Counter(
                tuple(r['arm'] for r in schedule if r['block'] == b and r['workload']['name'] == workload)
                for b in range(12))
            self.assertEqual(len(orders), 6)
            self.assertEqual(set(orders.values()), {2})
        prefixes = [r['command'][r['command'].index('--prefix') + 1] for r in schedule]
        self.assertEqual(len(set(prefixes)), len(schedule))
        self.assertEqual(len({len(p.encode()) for p in prefixes}), 1)
        revisions = {r['arm']: r['command'][r['command'].index('--source-revision') + 1] for r in schedule}
        self.assertEqual(revisions, {'g9a': 'a' * 40, 'g267': 'b' * 40, 'cgo': 'b' * 40})
        self.assertEqual(plan['environment']['GOMAXPROCS'], '8')
        self.assertEqual(plan['campaign_wall_cap_seconds'], 180)
        for arm in plan['arms']:
            self.assertEqual(plan['expected_sha256'][arm['binary']], r3_plan.digest(arm['binary']))
        self.assertIn(str(Path(matrix_plan.__file__).resolve()), plan['pinned_files'])

    def test_rejected_arguments(self):
        cases = {
            'blocks_not_counterbalanced': dict(blocks=8),
            'no_cgo': dict(arm=self.args.arm[:2]),
            'one_arm': dict(arm=self.args.arm[2:]),
            'duplicate_name': dict(arm=[self.args.arm[0], self.args.arm[0].replace('before', 'after'), self.args.arm[2]]),
            'shared_binary': dict(arm=[self.args.arm[0], self.args.arm[1].replace('after', 'before'), self.args.arm[2]]),
            'short_revision': dict(arm=[self.args.arm[0].replace('a' * 40, 'abc'), *self.args.arm[1:]]),
            'unsafe_name': dict(arm=['../x' + self.args.arm[0][3:], *self.args.arm[1:]]),
            'overlapping_cpus': dict(client_cpus='3-7'),
            'duplicate_workload': dict(workload=['read1000', 'read1000']),
            'wrong_harness': dict(harness_id='0' * 64),
            'no_iterations': dict(iterations=0),
            'padded_names_collide': dict(arm=[f'go={self.root / "before"}@{"a" * 40}',
                                              f'go_={self.root / "after"}@{"b" * 40}', self.args.arm[2]]),
            'waits_exceed_cap': dict(blocks=24),
            'two_workloads_exceed_cap': dict(workload=['read1000', 'range100']),
        }
        for name, change in cases.items():
            args = copy.copy(self.args)
            for key, value in change.items():
                setattr(args, key, value)
            with self.subTest(name), self.assertRaises(ValueError):
                matrix_plan.make_plan(args)

    def test_run_rejects_edited_arm_options(self):
        plan = matrix_plan.make_plan(self.args)
        command = plan['arms'][0]['command']
        command[command.index('--retry-limit') + 1] = '0'
        with self.assertRaises(ValueError):
            run.make_schedule(plan, self.root / 'out')


if __name__ == '__main__':
    unittest.main()
