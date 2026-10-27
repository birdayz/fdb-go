"""Publication and CPU-only failure boundaries with hash-pinned synthetic inputs."""
import copy
from decimal import Decimal
import hashlib
import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import export_results as export
import test_export_results as fixtures
import test_server_cpu


def cpu_fixture(phase='M2rl'):
    layout, units, telemetry, _, run = test_server_cpu.fixture()
    case = 'grv' if phase.startswith('M1') else 'small-limit1-forward'
    report = dict(cases=[dict(case=case, verdict='clean', runs=[])],
                  nodes={node: dict(units=sorted(units[node])) for node in units},
                  clock=dict(client=dict(bound_s='0')), inputs_sha256={})
    values = {}
    for node, (start, stop, samples) in telemetry.items():
        start['phase'] = stop['phase'] = phase
        start['chrony'] = stop['chrony'] = dict(rc=0, raw='synthetic synced chrony')
        root = f'telemetry-cpu/{phase}/{node}'
        values[root + '/samples.jsonl'] = ''.join(json.dumps(sample) + '\n' for sample in samples)
        stop['samples_sha256'] = hashlib.sha256(values[root + '/samples.jsonl'].encode()).hexdigest()
        for kind, receipt in (('start', start), ('stop', stop)):
            values[root + f'/receipt-{kind}.json'] = receipt
            values[f'telemetry/{phase}/{node}/receipt-{kind}.json'] = copy.deepcopy(receipt)
    inputs = fixtures.Inputs(values)
    for path in list(values):
        if path.startswith('telemetry/'):
            report['inputs_sha256']['/srv/fdb-bench/' + path] = inputs.digest(path)
    rows = []
    for arm in ('before', 'final', 'cgo') if phase.startswith('M1') else ('final-ordinary', 'final-remote'):
        report['cases'][0]['runs'].append(dict(run, run_id=arm))
        rows.append(dict(phase=phase, arm=arm, block=0, source_run_id=arm, measured_ops=100))
        folder = f'results-raw/{phase}/' + (case + '/' if phase.startswith('M1') else '') + arm
        values[folder + '/result.json'] = dict(run_id=arm, started_utc=run['start_utc'], ended_utc=run['end_utc'])
        if phase.startswith('M1'):
            values[folder + '/summary.json'] = dict(measured_fences_format='fdb-bench-measured-fences/v1',
                measured_started_unix_ns=1_200_000_000, measured_ended_unix_ns=1_400_000_000,
                measured_started_utc='1970-01-01T00:00:01.2Z', measured_ended_utc='1970-01-01T00:00:01.4Z',
                measured_wall_ns=200_000_000)
    parser = SimpleNamespace(parse_chrony_tracking=lambda text: ({}, []), chrony_error_bound=lambda fields: Decimal('0'))
    return inputs, report, layout, rows, parser


class SafetyTests(unittest.TestCase):
    def test_real_private_r4_diagnostics_publish_safe_categories_with_pinned_identity(self):
        inputs, _, expected = fixtures.population_fixture()
        reasons = ['role or process-class change at 10.89.20.2:4500',
                   'missing receipt /srv/fdb-bench/private-host/receipt.json',
                   '/home/private/token hcloud_private_resource']
        rec = inputs.values['reconcile/M1w32/report.json']['cases'][0]
        rec.update(verdict='contaminated', contaminated_reasons=reasons)
        inputs.values['analysis/current/M1w32/summary.json']['workloads']['grv']['status'] = 'N/A'
        inputs.repin_analysis()
        fixtures.seal_export_fixture(inputs, expected, clean=43)
        with patch.object(export, 'collect_m1', return_value=([], [])), \
             patch.object(export.m2_export, 'collect', return_value=([], {})), \
             patch.object(export, 'cpu_rows', return_value=[]):
            result = export.export(inputs, 'analysis/current', None, None, None)
        self.assertTrue(result['performance_results_available'])
        self.assertEqual(result['format'], 'fdbmn-rfc282-public-results/v2')
        self.assertEqual(result['server_cgroup_accounting'], [])
        self.assertNotIn('server_process_cpu', result)
        self.assertEqual((result['coverage']['clean'], result['coverage']['contaminated']), (43, 1))
        public = result['coverage']['phases']['M1w32']['grv']['reasons']
        self.assertEqual(len(public), 3)
        self.assertTrue(public[0].startswith('FDB role or process-class changed '))
        self.assertTrue(public[1].startswith('required evidence missing '))
        for reason, published in zip(reasons, public):
            self.assertTrue(published.endswith('[sha256:' + hashlib.sha256(reason.encode()).hexdigest() + ']'))
            self.assertNotIn(reason, json.dumps(result))
        export.legacy_io.validate_public({'results.json': json.dumps(result) + '\n'},
                                        {'identifiers': ['private-host'], 'names': ['private_resource']})
        self.assertEqual(inputs.values['reconcile/M1w32/report.json']['cases'][0]['contaminated_reasons'], reasons)

    def test_wrong_shape_hash_matching_cpu_samples_disqualify_only_cpu(self):
        for mutation in ('null_record', 'list_record', 'null_process', 'list_process', 'null_cgroup', 'null_procs',
                         'null_start', 'null_chrony', 'null_units', 'null_mount'):
            with self.subTest(mutation=mutation):
                inputs, report, layout, rows, parser = cpu_fixture()
                good = export.cpu_rows(inputs, 'M2rl', report, layout, rows, parser)
                self.assertEqual([row['status'] for row in good], ['observed', 'observed'])
                for row in good:
                    fixtures.assert_accounting_only(self, row)
                root = 'telemetry-cpu/M2rl/node1'
                samples = [json.loads(line) for line in inputs.values[root + '/samples.jsonl'].splitlines()]
                if mutation == 'null_record': samples[20] = None
                if mutation == 'list_record': samples[20] = []
                for label, field, value in (('null_process', 'process_cpu', None), ('list_process', 'process_cpu', []),
                                             ('null_cgroup', 'cgroup_cpu', None), ('null_procs', 'procs', None)):
                    if mutation == label: samples[20][field] = value
                text = ''.join(json.dumps(sample) + '\n' for sample in samples)
                inputs.values[root + '/samples.jsonl'] = text
                inputs.values[root + '/receipt-stop.json']['samples_sha256'] = hashlib.sha256(text.encode()).hexdigest()
                if mutation == 'null_start': inputs.values[root + '/receipt-start.json'] = None
                for label, field in (('null_chrony', 'chrony'), ('null_units', 'units'), ('null_mount', 'cgroup_v2')):
                    if mutation == label: inputs.values[root + '/receipt-start.json'][field] = None
                before = copy.deepcopy(report)
                output = export.cpu_rows(inputs, 'M2rl', report, layout, rows, parser)
                self.assertEqual(len(output), 1)
                self.assertEqual((output[0]['case'], output[0]['status']), ('small-limit1-forward', 'CPU-unavailable'))
                fixtures.assert_accounting_only(self, output[0])
                self.assertNotIn('observed_accounted_cpu_usec', output[0])
                self.assertNotIn('observed_accounted_cpu_seconds', output[0])
                self.assertNotIn('nodes', output[0])
                self.assertEqual(report, before)
                export.legacy_io.validate_public({'results.json': json.dumps(output) + '\n'},
                                                {'identifiers': ['synthetic-private'], 'names': []})

    def test_malformed_last_arm_fence_omits_whole_case_without_private_diagnostics(self):
        for value in (None, [], {}, 1):
            with self.subTest(value=value):
                inputs, report, layout, rows, parser = cpu_fixture('M1w1')
                positive = export.cpu_rows(inputs, 'M1w1', report, layout, rows, parser)
                self.assertEqual(len(positive), 3)
                self.assertEqual([row['status'] for row in positive], ['observed'] * 3)
                for row in positive:
                    fixtures.assert_accounting_only(self, row)
                inputs.values['results-raw/M1w1/grv/cgo/summary.json']['measured_started_utc'] = value
                output = export.cpu_rows(inputs, 'M1w1', report, layout, rows, parser)
                self.assertEqual(len(output), 1)
                self.assertEqual((output[0]['case'], output[0]['status']), ('grv', 'CPU-unavailable'))
                self.assertIn('window timestamp is not explicit UTC', output[0]['reason'])
                fixtures.assert_accounting_only(self, output[0])
                self.assertNotIn('observed_accounted_cpu_usec', output[0])
                self.assertNotIn('observed_accounted_cpu_seconds', output[0])
                self.assertNotIn('nodes', output[0])


if __name__ == '__main__':
    unittest.main()
