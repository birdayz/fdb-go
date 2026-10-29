import copy
from decimal import Decimal
import json
from pathlib import Path
import unittest

import server_cpu as cpu


def fixture():
    layout = dict(processes=[dict(node=node, port=4500 + index) for node, count in
                            zip(cpu.NODES, (7, 5, 5)) for index in range(count)])
    units = cpu.unit_population(layout)
    telemetry, nodes = {}, {}
    for node in cpu.NODES:
        identity = {unit: dict(pid=100 + i, starttime='1234') for i, unit in enumerate(sorted(units[node]))}
        start = dict(host=node, phase='M1w1', kind='start', process_cpu_format=cpu.FORMAT,
                     clock_ticks_per_second=100, boot_id='boot', units=dict(names=sorted(units[node]), starttimes=identity))
        groups = {unit: dict(path='/sys/fs/cgroup/system.slice/' + unit, **p, procs=[p['pid']],
                            threads=[p['pid'], p['pid'] + 1000], device=1, inode=p['pid'])
                  for unit, p in identity.items()}
        start.update(cgroup_cpu_format=cpu.CGROUP_FORMAT, cgroup_units=groups,
                     cgroup_v2=dict(path='/sys/fs/cgroup', device=1, inode=1, controllers=['cpu', 'cpuset']))
        stop = copy.deepcopy(start)
        stop.update(kind='stop', samples=101)
        samples = []
        for seq in range(101):
            stamp = 1_000_000_000 + seq * 10_000_000
            counters = {unit: dict(**value, user_ticks=seq, system_ticks=0) for unit, value in identity.items()}
            samples.append(dict(seq=seq, process_cpu_format=cpu.FORMAT, process_cpu=counters,
                                procs={u: p['starttime'] for u, p in identity.items()},
                                cgroup_cpu_format=cpu.CGROUP_FORMAT,
                                cgroup_cpu={unit: dict(**g, usage_usec=seq * 10000,
                                                      user_usec=seq * 8000, system_usec=seq * 2000)
                                            for unit, g in groups.items()},
                                realtime_ns=stamp, realtime_after_ns=stamp, monotonic_ns=stamp))
        telemetry[node] = (start, stop, samples)
        nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node], max_gap_ns=20_000_000)
    run = dict(start_utc='1970-01-01T00:00:01.1Z', end_utc='1970-01-01T00:00:01.9Z', nodes={})
    for node in cpu.NODES:
        run['nodes'][node] = dict(u_s='1.001', widened_start_ns=99_000_000, widened_end_ns=2_901_000_000)
    return layout, units, telemetry, nodes, run


class Tests(unittest.TestCase):
    def test_known_seventeen_group_accounting_deltas_not_execution_bounds(self):
        _, _, _, nodes, run = fixture()
        result = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(result['observed_accounted_cpu_usec'], 3_740_000)
        self.assertAlmostEqual(result['observed_accounted_cpu_seconds'], 3.74)
        self.assertEqual([result['nodes'][n]['processes'] for n in cpu.NODES], [7, 5, 5])
        self.assertFalse(result['actual_window_cpu_available'])
        self.assertEqual(result['window_kind'], 'measured-client-UTC-fences')
        self.assertEqual((result['requested_window_started_unix_ns'], result['requested_window_ended_unix_ns']),
                         (1_200_000_000, 1_400_000_000))
        self.assertIn('scheduler accounting debt is unquantified', result['scope'])
        self.assertEqual(result['nodes']['node1']['temporal_selection_allowance_ns'], 2_000_001)
        for value in result['nodes'].values():
            self.assertEqual((value['sample_bracket_started_unix_ns'], value['sample_bracket_ended_unix_ns'],
                              value['sample_bracket_wall_ns']), (1_190_000_000, 1_410_000_000, 220_000_000))
            self.assertEqual(value['observed_accounted_cpu_usec'] / 1e6, value['observed_accounted_cpu_seconds'])
        for value in [result, *result['nodes'].values()]:
            self.assertTrue(set(value).isdisjoint({'lower_cpu_seconds', 'upper_cpu_seconds', 'bracket_cores',
                                                   'lower_cpu_ns_per_op', 'upper_cpu_ns_per_op', 'ratio',
                                                   'ci95_low', 'ci95_high', 'cpu_util_cores'}))

    def test_frozen_section_example_matches_emitted_observation_and_closed_schema_fields(self):
        folder = Path(__file__).resolve().parent
        example = json.loads((folder / 'accounting-example.json').read_text())
        schema = json.loads((folder / 'accounting-contract.schema.json').read_text())
        _, _, _, nodes, run = fixture()
        observed = dict(phase='M1w1', case='grv', arm='before', block=0, status='observed',
                        **cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000)))
        self.assertEqual(example['format'], 'fdbmn-rfc282-public-results/v2')
        self.assertEqual(schema['properties']['format']['const'], example['format'])
        self.assertEqual(example['server_cgroup_accounting'][0], observed)
        for name, value in [('observed', observed), ('unavailable', example['server_cgroup_accounting'][1]),
                            ('method', example['method']['server_cpu']),
                            *[('node', node) for node in observed['nodes'].values()]]:
            definition = schema['$defs'][name]
            self.assertFalse(definition['additionalProperties'])
            self.assertEqual(set(definition['required']), set(value))
            self.assertEqual(set(definition['properties']), set(value))

    def test_delayed_scheduler_publication_is_observation_not_execution_attribution(self):
        _, units, telemetry, _, run = fixture()
        nodes = {}
        for node, (start, stop, samples) in telemetry.items():
            charged_unit = sorted(units[node])[0]
            for sample in samples:
                for unit, group in sample['cgroup_cpu'].items():
                    charge = 9000 if node == 'node1' and unit == charged_unit and sample['seq'] >= 41 else 0
                    group.update(usage_usec=charge, user_usec=charge, system_usec=0)
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        # F6's schedule runs1.3966..1.4056 but publishes9ms at1.41. The
        # earlier window sees0; a later window sees9ms of delayed accounting.
        earlier = cpu.observe_run(run, nodes, (1_200_000_000, 1_397_999_000))
        later = cpu.observe_run(run, nodes, (1_407_000_000, 1_607_000_000))
        self.assertEqual(earlier['observed_accounted_cpu_usec'], 0)
        self.assertEqual(later['observed_accounted_cpu_usec'], 9000)
        self.assertEqual(earlier['nodes']['node1']['sample_sequence'], [19, 40])
        for result in (earlier, later):
            self.assertFalse(result['actual_window_cpu_available'])
            for value in [result, *result['nodes'].values()]:
                self.assertNotIn('lower_cpu_seconds', value)
                self.assertNotIn('upper_cpu_seconds', value)
                self.assertNotIn('bracket_cores', value)
                self.assertNotIn('lower_cpu_ns_per_op', value)

    def test_fleet_sums_deltas_over_distinct_node_sample_brackets_without_common_wall(self):
        _, units, telemetry, _, run = fixture()
        nodes = {}
        for (node, (start, stop, samples)), shift in zip(telemetry.items(), (0, 3_000_000, 7_000_000)):
            for sample in samples:
                for field in ('realtime_ns', 'realtime_after_ns', 'monotonic_ns'): sample[field] += shift
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        result = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(result['observed_accounted_cpu_usec'], 3_640_000)
        expected = {'node1': (1_540_000, 1_190_000_000, 1_410_000_000),
                    'node2': (1_050_000, 1_193_000_000, 1_403_000_000),
                    'node3': (1_050_000, 1_197_000_000, 1_407_000_000)}
        for node, (usage, start, end) in expected.items():
            value = result['nodes'][node]
            self.assertEqual((value['observed_accounted_cpu_usec'], value['sample_bracket_started_unix_ns'],
                              value['sample_bracket_ended_unix_ns'], value['sample_bracket_wall_ns']),
                             (usage, start, end, end - start))
        for field in ('sample_bracket_wall_ns', 'sample_bracket_started_unix_ns',
                      'sample_bracket_ended_unix_ns', 'bracket_cores', 'rate'):
            self.assertNotIn(field, result)

    def test_raw_cgroup_deltas_differ_from_proc_and_component_diagnostics(self):
        _, units, telemetry, _, run = fixture()
        nodes = {}
        for node, (start, stop, samples) in telemetry.items():
            for sample in samples:
                seq = sample['seq']
                for process in sample['process_cpu'].values():
                    process.update(user_ticks=seq * 3, system_ticks=seq * 2)
                for group in sample['cgroup_cpu'].values():
                    group.update(usage_usec=seq * 7000, user_usec=seq * 1000, system_usec=seq * 2000)
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        result = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(result['observed_accounted_cpu_usec'], 2_618_000)
        self.assertAlmostEqual(result['observed_accounted_cpu_seconds'], 2.618)
        self.assertAlmostEqual(sum(value['observed_accounted_user_cpu_seconds'] for value in result['nodes'].values()), .374)
        self.assertAlmostEqual(sum(value['observed_accounted_system_cpu_seconds'] for value in result['nodes'].values()), .748)
        for node, (start, stop, samples) in telemetry.items():
            for sample in samples:
                for group in sample['cgroup_cpu'].values():
                    group.update(usage_usec=0, user_usec=0, system_usec=0)
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        zero = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(zero['observed_accounted_cpu_seconds'], 0)
        self.assertEqual(zero['observed_accounted_cpu_usec'], 0)

    def test_user_and_system_are_added_once_no_child_or_host_cpu(self):
        _, units, telemetry, _, run = fixture()
        nodes = {}
        for node, (start, stop, samples) in telemetry.items():
            for sample in samples:
                for process in sample['process_cpu'].values():
                    process['system_ticks'] = process['user_ticks']
                    process['child_ticks'] = 999999999  # never part of estimator
                for group in sample['cgroup_cpu'].values():
                    for field in ('usage_usec', 'user_usec', 'system_usec'):
                        group[field] *= 2
                sample['stat'] = 'irrelevant host workload/guest/IRQ counters'
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        self.assertAlmostEqual(cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))['observed_accounted_cpu_seconds'], 7.48)

    def test_different_hz_and_zero_cpu_have_defined_values_not_missing_zeros(self):
        _, units, telemetry, _, run = fixture()
        nodes = {}
        for i, (node, (start, stop, samples)) in enumerate(telemetry.items()):
            start['clock_ticks_per_second'] = stop['clock_ticks_per_second'] = 100 * (i + 1)
            for sample in samples:
                for process in sample['process_cpu'].values():
                    process['user_ticks'] *= i + 1
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        self.assertAlmostEqual(cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))['observed_accounted_cpu_seconds'], 3.74)
        for data in nodes.values():
            for interval in data['intervals']:
                interval.update(usage_usec=0, user_usec=0, system_usec=0)
        result = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(result['observed_accounted_cpu_usec'], 0)
        self.assertFalse(result['actual_window_cpu_available'])

    def test_pid_restart_reuse_foreign_missing_and_bad_ticks_refuse(self):
        for mutation in ('restart', 'reuse', 'missing', 'foreign', 'negative', 'decrease', 'float', 'error'):
            with self.subTest(mutation=mutation):
                _, units, telemetry, _, _ = fixture()
                start, stop, samples = telemetry['node1']
                unit = sorted(units['node1'])[0]
                process = samples[20]['process_cpu'][unit]
                if mutation == 'restart': stop['units']['starttimes'][unit]['pid'] += 100
                if mutation == 'reuse': process['starttime'] = '5678'
                if mutation == 'missing': samples[20]['process_cpu'].pop(unit)
                if mutation == 'foreign': samples[20]['process_cpu']['foreign'] = process
                if mutation == 'negative': process['user_ticks'] = -1
                if mutation == 'decrease': process['user_ticks'] = 0
                if mutation == 'float': process['user_ticks'] = 20.0
                if mutation == 'error': samples[20]['process_cpu_errors'] = {unit: 'read failed'}
                with self.assertRaises(ValueError): cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'])

    def test_phase_wide_malformed_clock_gap_counter_receipt_and_population_controls(self):
        for mutation in ('sequence', 'clock_step', 'read_overlap', 'hz', 'changed_hz', 'boot', 'old_sample', 'empty'):
            with self.subTest(mutation=mutation):
                _, units, telemetry, _, _ = fixture()
                start, stop, samples = telemetry['node1']
                if mutation == 'sequence': samples[20]['seq'] = 19
                if mutation == 'clock_step': samples[20]['realtime_ns'] += 2_000_000
                if mutation == 'read_overlap': samples[20]['realtime_after_ns'] += 20_000_000
                if mutation == 'hz': start['clock_ticks_per_second'] = 0
                if mutation == 'changed_hz': stop['clock_ticks_per_second'] = 1000
                if mutation == 'boot': stop['boot_id'] = 'different'
                if mutation == 'old_sample': samples[20].pop('process_cpu_format')
                if mutation == 'empty': samples.clear()
                with self.assertRaises(ValueError): cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'])

    def test_gap_limit_is_independent_of_clock_correlation_and_inclusive(self):
        for shift, accepted in ((10_000_000, True), (10_000_001, False)):
            _, units, telemetry, _, _ = fixture()
            start, stop, samples = telemetry['node1']
            for sample in samples[20:]:
                for field in ('realtime_ns', 'realtime_after_ns', 'monotonic_ns'):
                    sample[field] += shift
            if accepted:
                data = cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'], max_gap_ns=20_000_000)
                self.assertEqual(data['correlation_range_ns'], 0)
            else:
                with self.assertRaisesRegex(cpu.CPUUnqualified, 'CPU gap or overlapping sample reads'):
                    cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'], max_gap_ns=20_000_000)

    def test_exclusivity_guards_have_consistent_receipt_and_sample_identities(self):
        for mutation, reason in (('pid', 'shared PID double count'), ('group', 'shared cgroup double count'),
                                 ('controller', 'missing/changed cgroup v2 CPU mount')):
            _, units, telemetry, _, _ = fixture()
            start, stop, samples = telemetry['node1']
            first, second = sorted(units['node1'])[:2]
            for receipt in (start, stop):
                if mutation == 'pid':
                    pid = receipt['units']['starttimes'][first]['pid']
                    receipt['units']['starttimes'][second]['pid'] = pid
                    receipt['cgroup_units'][second].update(pid=pid, procs=[pid], threads=[pid, pid + 1000])
                if mutation == 'group':
                    receipt['cgroup_units'][second]['inode'] = receipt['cgroup_units'][first]['inode']
                if mutation == 'controller': receipt['cgroup_v2']['controllers'] = ['memory']
            for sample in samples:
                if mutation == 'pid': sample['process_cpu'][second]['pid'] = start['units']['starttimes'][first]['pid']
                sample['cgroup_cpu'][second].update(start['cgroup_units'][second])
            with self.assertRaisesRegex(cpu.CPUUnqualified, reason):
                cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'])

    def test_m2_lifecycle_retains_timestamp_precision_reserve(self):
        _, _, _, nodes, run = fixture()
        result = cpu.observe_run(run, nodes)
        self.assertEqual(result['nodes']['node1']['temporal_selection_allowance_ns'], 3_000_000)
        self.assertEqual(result['window_kind'], 'process-lifecycle-UTC')
        self.assertNotIn('lower_cpu_ns_per_op', result)

    def test_short_windows_and_clock_uncertainty_are_visible_unqualified_not_interpolated(self):
        _, _, _, nodes, run = fixture()
        # 130ms outer / 110ms measured passes20%; exactly nine inner intervals fails ten.
        with self.assertRaisesRegex(ValueError, 'resolution unqualified'):
            cpu.observe_run(run, nodes, (1_200_000_000, 1_310_000_000))
        accepted = cpu.observe_run(run, nodes, (1_200_000_000, 1_320_000_000))
        self.assertEqual(accepted['nodes']['node1']['inner_intervals'], 10)
        for data in nodes.values(): data['clock_bound_s'] = Decimal('0.2')
        with self.assertRaisesRegex(ValueError, 'resolution unqualified'):
            cpu.observe_run(run, nodes, (1_300_000_000, 1_500_000_000))

    def test_windows_outside_lifecycle_bad_r4_bounds_missing_nodes_and_coverage_refuse(self):
        for mutation in ('lifecycle', 'r4', 'node', 'coverage'):
            _, _, _, nodes, run = fixture()
            window = (1_200_000_000, 1_400_000_000)
            if mutation == 'lifecycle': window = (1_000_000_000, 1_200_000_000)
            if mutation == 'r4': run['nodes']['node1']['widened_start_ns'] += 1
            if mutation == 'node': run['nodes'].pop('node3')
            if mutation == 'coverage': nodes['node1']['intervals'] = nodes['node1']['intervals'][40:]
            with self.assertRaises(ValueError): cpu.observe_run(run, nodes, window)

    def test_exact_twenty_percent_outer_time_envelope_is_inclusive(self):
        _, _, _, nodes, run = fixture()
        for data in nodes.values(): data['clock_bound_s'] = Decimal('0.01')
        result = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(result['nodes']['node1']['sample_bracket_wall_ns'], 240_000_000)
        for data in nodes.values(): data['clock_bound_s'] = Decimal('0.02')
        with self.assertRaisesRegex(ValueError, 'resolution unqualified'):
            cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))

    def test_accumulated_sub_millisecond_clock_steps_fail_phase_range(self):
        _, units, telemetry, _, _ = fixture()
        start, stop, samples = telemetry['node1']
        once = copy.deepcopy(samples)
        for sample in once[30:]:
            sample['realtime_ns'] += 600_000
            sample['realtime_after_ns'] += 600_000
        data = cpu.validate_node(start, stop, once, 'node1', 'M1w1', units['node1'])
        self.assertEqual(data['correlation_range_ns'], 600_000)  # one allowed step
        accumulated = copy.deepcopy(samples)
        for sample in accumulated[30:]:
            shift = 600_000 * min(sample['seq'] - 29, 2)
            sample['realtime_ns'] += shift
            sample['realtime_after_ns'] += shift
        # Every adjacent difference is <=0.6ms, but the phase range is1.2ms.
        self.assertTrue(all(abs((b['realtime_ns'] - a['realtime_ns']) -
                               (b['monotonic_ns'] - a['monotonic_ns'])) <= 600_000
                            for a, b in zip(accumulated, accumulated[1:])))
        with self.assertRaisesRegex(ValueError, 'phase CPU clock correlation range'):
            cpu.validate_node(start, stop, accumulated, 'node1', 'M1w1', units['node1'])

    def test_edge_counter_read_brackets_not_just_sample_start_end_must_cover(self):
        for edge in ('start', 'end'):
            for qualified in (True, False):
                with self.subTest(edge=edge, qualified=qualified):
                    _, units, telemetry, nodes, run = fixture()
                    start, stop, samples = telemetry['node1']
                    samples = samples[19:42]
                    if edge == 'start':
                        samples[0]['realtime_after_ns'] = 1_197_000_000 if qualified else 1_198_000_000
                    else:
                        stamp = 1_402_000_001 if qualified else 1_401_000_000
                        samples[-1].update(realtime_ns=stamp, monotonic_ns=stamp, realtime_after_ns=1_403_000_000)
                    for seq, sample in enumerate(samples): sample['seq'] = seq
                    stop['samples'] = len(samples)
                    data = cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'], max_gap_ns=20_000_000)
                    self.assertEqual(data['coverage_start_ns'], samples[0]['realtime_after_ns'])
                    self.assertEqual(data['coverage_end_ns'], samples[-1]['realtime_ns'])
                    nodes['node1'] = data
                    if qualified:
                        cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
                    else:
                        with self.assertRaisesRegex(cpu.CPUUnqualified, 'certainly bracket'):
                            cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))

    def test_cgroup_missing_foreign_membership_reset_and_recreation_refuse(self):
        for mutation in ('missing', 'foreign_path', 'foreign_process', 'foreign_thread', 'reset', 'negative',
                         'units', 'recreate', 'components', 'component_reset'):
            with self.subTest(mutation=mutation):
                _, units, telemetry, _, _ = fixture()
                start, stop, samples = telemetry['node1']
                unit = sorted(units['node1'])[0]
                group = samples[20]['cgroup_cpu'][unit]
                if mutation == 'missing': samples[20]['cgroup_cpu'].pop(unit)
                if mutation == 'foreign_path': group['path'] = '/sys/fs/cgroup/system.slice/ssh.service'
                if mutation == 'foreign_process': group['procs'] = [group['pid'], 9999]
                if mutation == 'foreign_thread': group['threads'] = group['threads'] + [9999]
                if mutation == 'reset':
                    group.update(usage_usec=0, user_usec=0, system_usec=0)
                if mutation == 'negative': group['usage_usec'] = -1
                if mutation == 'units': group['usage_usec'] = group['usage_usec'] / 1e6
                if mutation == 'recreate': group['inode'] += 1
                if mutation == 'components': group.pop('system_usec')
                if mutation == 'component_reset':
                    group.update(user_usec=0, system_usec=group['usage_usec'])
                with self.assertRaises(ValueError):
                    cpu.validate_node(start, stop, samples, 'node1', 'M1w1', units['node1'])

    def test_cgroup_user_system_optional_not_fabricated_zero(self):
        _, units, telemetry, _, run = fixture()
        nodes = {}
        for node, (start, stop, samples) in telemetry.items():
            for sample in samples:
                for group in sample['cgroup_cpu'].values():
                    group.pop('user_usec')
                    group.pop('system_usec')
            nodes[node] = cpu.validate_node(start, stop, samples, node, 'M1w1', units[node])
        result = cpu.observe_run(run, nodes, (1_200_000_000, 1_400_000_000))
        self.assertEqual(result['nodes']['node1']['observed_accounted_user_cpu_seconds'], None)
        self.assertAlmostEqual(result['observed_accounted_cpu_seconds'], 3.74)

    def test_nanosecond_utc_is_exact_above_float_mantissa(self):
        self.assertEqual(cpu.utc_ns('2026-10-20T13:13:35.425685123Z'), 1792502015425685123)
        self.assertEqual(cpu.utc_ns('2026-10-20T13:13:35.425685123+00:00'), 1792502015425685123)
        with self.assertRaises(ValueError): cpu.utc_ns('2026-10-20T13:13:35.425685123')


if __name__ == '__main__':
    unittest.main()
