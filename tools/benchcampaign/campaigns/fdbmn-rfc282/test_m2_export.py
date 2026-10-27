import copy
import unittest
from unittest.mock import patch

from helpers import legacy_m2
import test_m2_throughput as fixture_source
import m2_export


class Tests(unittest.TestCase):
    def collect(self, counts):
        with patch.object(legacy_m2, 'PHASES', counts):
            inputs = fixture_source.fixture()
        expected = {phase: [c['key'] for c in inputs.json[f'results-raw/{phase}/plan.json']['cases']] for phase in counts}
        def paired(ratios, *, resamples, seed):
            self.assertEqual((len(ratios), resamples, seed), (6, 20000, 492817))
            return dict(gm=2, nominal_95_interval=[1.5, 2.5], blocks=6, resamples=resamples, seed=seed)
        return m2_export.collect(inputs, paired, expected, 'analysis/measured-492817')

    def test_full_sixteen_clean_not_historical_twelve(self):
        rows, method = self.collect({'M2rl': (12, 12), 'M2skip': (4, 4)})
        self.assertEqual(method['eligible_cases'], 16)
        self.assertEqual(method['process_rows'], 192)
        self.assertEqual(method['source_repeat_samples'], 576)
        self.assertEqual(sum(r['record_type'] == 'omitted' for r in rows), 0)

    def test_arbitrary_omissions_no_partial_process_salvage(self):
        rows, method = self.collect({'M2rl': (12, 2), 'M2skip': (4, 1)})
        self.assertEqual((method['eligible_cases'], method['omitted_cases'], method['process_rows']), (3, 13, 36))
        self.assertTrue(all(r['source_processes'] == 1 for r in rows if r['record_type'] == 'process'))

    def test_zero_clean_emits_only_omission_rows_not_estimators(self):
        rows, method = self.collect({'M2rl': (12, 0), 'M2skip': (4, 0)})
        self.assertEqual((method['eligible_cases'], method['process_rows']), (0, 0))
        self.assertEqual(len(rows), 16)
        self.assertEqual({r['record_type'] for r in rows}, {'omitted'})

    def test_foreign_expected_population_refused(self):
        inputs = fixture_source.fixture()
        with self.assertRaises(ValueError):
            m2_export.collect(inputs, lambda *a, **k: self.fail('must not estimate'),
                              {'M2rl': ['foreign'], 'M2skip': ['foreign']}, 'analysis/measured-492817')


if __name__ == '__main__':
    unittest.main()
