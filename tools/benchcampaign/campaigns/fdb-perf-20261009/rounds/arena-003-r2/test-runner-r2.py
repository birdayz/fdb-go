#!/usr/bin/env python3
import copy
import importlib.util
import json
from pathlib import Path
import unittest
here = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('runner', '/opt/fdbperf/scripts/astra-perf-attribution-r2.py')
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)
plan = json.loads((here / 'r2-cpu-plan.json').read_text())
class Attribution(unittest.TestCase):
    def test_profile_positive(self):
        runner.validate_campaign_kind(plan)
    def test_missing_authorization_flag(self):
        p = copy.deepcopy(plan)
        del p['attribution_only']
        with self.assertRaises(AssertionError): runner.validate_campaign_kind(p)
    def test_unprofiled_rejected(self):
        p = copy.deepcopy(plan)
        command = p['arms'][0]['command']
        command[command.index('--profile')+1] = 'none'
        with self.assertRaises(AssertionError): runner.validate_campaign_kind(p)
    def test_multiarm_attribution_rejected(self):
        p = copy.deepcopy(plan)
        p['arms'].append(copy.deepcopy(p['arms'][0]))
        with self.assertRaises(AssertionError): runner.validate_campaign_kind(p)
    def test_multiblock_attribution_rejected(self):
        p = copy.deepcopy(plan)
        p['blocks'] = 2
        with self.assertRaises(AssertionError): runner.validate_campaign_kind(p)
    def test_original_comparison_rule_retained(self):
        p = {'arms': [{}, {}, {}], 'blocks': 6}
        runner.validate_campaign_kind(p)
        p['blocks'] = 5
        with self.assertRaises(AssertionError): runner.validate_campaign_kind(p)
if __name__ == '__main__': unittest.main(verbosity=2)
