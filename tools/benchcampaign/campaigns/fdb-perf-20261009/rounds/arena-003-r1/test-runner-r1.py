#!/usr/bin/env python3
import copy
import importlib.util
import itertools
import json
from pathlib import Path
import unittest

here = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('runner', '/opt/fdbperf/scripts/astra-perf-run-r1.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
plan = json.loads((here / 'r1-plan.json').read_text())

class Orders(unittest.TestCase):
    def test_r1_positive(self):
        orders = runner.counterbalanced_orders(plan)
        self.assertEqual(len(orders), 6)
        self.assertEqual(len(set(orders)), 6)
        self.assertTrue(all(set(x) == set(range(4)) for x in orders))
        for a, b in itertools.combinations(range(4), 2):
            self.assertEqual(sum(o.index(a) < o.index(b) for o in orders), 3)

    def test_original_three_arm_full_factorial(self):
        p = {'arms': [{'name': a} for a in ['old', 'new', 'cgo']], 'blocks': 6}
        self.assertEqual(set(runner.counterbalanced_orders(p)), set(itertools.permutations(range(3))))

    def test_four_arm_implicit_six_rejected(self):
        p = copy.deepcopy(plan)
        del p['explicit_orders']
        with self.assertRaises(AssertionError):
            runner.counterbalanced_orders(p)

    def test_duplicate_rejected(self):
        p = copy.deepcopy(plan)
        p['explicit_orders'][0] = p['explicit_orders'][1]
        with self.assertRaises(AssertionError):
            runner.counterbalanced_orders(p)

    def test_missing_arm_rejected(self):
        p = copy.deepcopy(plan)
        p['explicit_orders'][0][0] = 'old'
        with self.assertRaises(AssertionError):
            runner.counterbalanced_orders(p)

    def test_partial_schedule_rejected(self):
        p = copy.deepcopy(plan)
        p['explicit_orders'].pop()
        with self.assertRaises(AssertionError):
            runner.counterbalanced_orders(p)

    def test_unbalanced_precedence_rejected(self):
        p = copy.deepcopy(plan)
        p['explicit_orders'] = [list(x) for x in itertools.permutations(['cgo', 'old', 'old-aa', 'new'])][:6]
        with self.assertRaises(AssertionError):
            runner.counterbalanced_orders(p)

if __name__ == '__main__':
    unittest.main(verbosity=2)
