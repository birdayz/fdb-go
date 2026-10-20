#!/usr/bin/env python3
import copy
import importlib.util
import json
from pathlib import Path
import unittest
here = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('runner', '/opt/fdbperf/scripts/astra-perf-diagnostic-r3.py')
runner = importlib.util.module_from_spec(spec);spec.loader.exec_module(runner)
p = json.loads((here / 'r3-perf-plan.json').read_text())
class Schedule(unittest.TestCase):
    def test_perf_positive(self): self.assertEqual(runner.campaign_orders(p), [(0,1)])
    def test_strace_positive(self):
        q = json.loads((here / 'r3-strace-plan.json').read_text());self.assertEqual(runner.campaign_orders(q), [(0,1)])
    def test_uninstrumented_rejected(self):
        q = copy.deepcopy(p);q['instrumentation'] = 'none'
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
    def test_wrong_wrapper_rejected(self):
        q = copy.deepcopy(p);q['arms'][0]['command'][0] = 'taskset'
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
    def test_multiblock_diagnostic_rejected(self):
        q = copy.deepcopy(p);q['blocks'] = 6
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
    def test_comparison_rule_retained(self):
        q = json.loads((here / 'r3-timed-plan.json').read_text())
        orders = runner.campaign_orders(q)
        self.assertEqual(orders.count((0,1)), 3);self.assertEqual(orders.count((1,0)), 3)
        q['blocks'] = 1
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
if __name__ == '__main__': unittest.main(verbosity=2)
