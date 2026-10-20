#!/usr/bin/env python3
import copy
import importlib.util
import json
from pathlib import Path
import unittest
here = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('runner', '/opt/fdbperf/scripts/astra-perf-record-r4.py')
runner = importlib.util.module_from_spec(spec);spec.loader.exec_module(runner)
p = json.loads((here / 'r4-fp-plan.json').read_text())
class Schedule(unittest.TestCase):
    def test_fp_positive(self): self.assertEqual(runner.campaign_orders(p), [(0,1)])
    def test_cgo_dwarf_positive(self):
        q = json.loads((here / 'r4-dwarf-plan.json').read_text());self.assertEqual(runner.campaign_orders(q), [(0,)])
    def test_single_fp_rejected(self):
        q = copy.deepcopy(p);q['arms'] = [q['arms'][1]]
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
    def test_wrong_wrapper_rejected(self):
        q = copy.deepcopy(p);q['arms'][0]['command'][1] = 'stat'
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
    def test_wrong_graph_rejected(self):
        q = copy.deepcopy(p);q['call_graph'] = 'dwarf'
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
    def test_multiblock_rejected(self):
        q = copy.deepcopy(p);q['blocks'] = 2
        with self.assertRaises(AssertionError): runner.campaign_orders(q)
if __name__ == '__main__': unittest.main(verbosity=2)
