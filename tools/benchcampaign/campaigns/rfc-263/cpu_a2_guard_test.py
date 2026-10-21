#!/usr/bin/env python3
"""Offline A2 fixtures; the owner runs them only in the reserved locked lane."""
from copy import deepcopy
import math
import statistics
import datetime as dt
from pathlib import Path
import shutil
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

sys.dont_write_bytecode = True
import cpu_collect as c
import cpu_analyze as a
import cpu_a2_spec as spec
import cpu_a2_admission as admission
import cpu_a2_analyze as subject
import cpu_guard_test as base

IDS = c.identities('1' * 40, spec.PRODUCTION_BIN)
A1_IDS = c.identities('2' * 40, spec.PRODUCTION_BIN)
NS = 'b' * 32


def retained_fixture():
    row = dict(point_ratio=1.0, lower_ratio=.99, upper_ratio=1.01, confidence=.95, epoch_counts=[6, 6])
    reports = {metric: {cell: deepcopy(row) for cell in a.CELL_NAMES} for metric in a.ENDPOINTS}
    for metric in a.ENDPOINTS:
        # A deliberately failing superseded point must not leak into A2's gate.
        reports[metric][spec.TARGET].update(point_ratio=1.2, lower_ratio=1.1, upper_ratio=1.3)
    return dict(seed=263007, processes=256, pairs=128, ab_pairs_per_cell=12, aa_pairs_per_cell=4,
                measured_transactions=66560000, warmup_transactions=512000, identities=deepcopy(A1_IDS),
                primary_and_coprimary=reports)


def retained_files(root, result=None):
    root.mkdir(parents=True, exist_ok=True)
    result = retained_fixture() if result is None else result
    results, index = root / 'results.json', root / 'artifact-sha256.json'
    c.save(results, result)
    result_pin = c.sha(results)
    c.save(index, {'results.json': result_pin})
    return results, index, result_pin, c.sha(index)


def confirmation_fixture(frozen_sha256='f' * 64, epoch_seals=None):
    retained = retained_fixture()
    target = {metric: dict(point_ratio=1, lower_ratio=.99, upper_ratio=1.01, confidence=.95, epoch_counts=[40, 40]) for metric in a.ENDPOINTS}
    combined, decision = subject.combine_reports(retained, target)
    return dict(timing_design=spec.DESIGN, seed=spec.SEED, processes=176, pairs=88, fresh_ab_pairs=80, fresh_aa_pairs=8,
        measured_transactions=3520000, warmup_transactions=352000, target_cell=spec.TARGET, target_ab_pairs=80,
        target_ab_pairs_per_epoch=[40, 40], aa_pairs_per_role=4, frozen_sha256=frozen_sha256,
        epoch_seals=epoch_seals or {'1': '1' * 64, '2': '2' * 64}, identities=IDS,
        target_decision=dict(verdict='PASS', park=False), decision=decision, target_primary_and_coprimary=target,
        primary_and_coprimary=combined, ab_pairs_by_cell={cell: 80 if cell == spec.TARGET else 12 for cell in a.CELL_NAMES},
        retained_a1=dict(frozen_sha256=spec.A1_FROZEN_SHA256, results_sha256=spec.RESULTS_SHA256, index_sha256=spec.INDEX_SHA256, bounds=14, cells=7, identities=A1_IDS))


def source_fixture():
    return dict(files={f'pkg/fixture{i}.go': {'sha256': 'f' * 64, 'mode': 33188, 'symlink': False} for i in range(1001)},
                external_configs={'fixture-user.bazelrc': None})


def prefix_batch(root, namespace, prior=None):
    root.mkdir()
    c.save(root / 'schedule.json', c.schedule(A1_IDS, namespace))
    c.save(root / 'schedule-diagnostics.json', c.diagnostic_schedule(A1_IDS, namespace))
    cluster = root / 'fixture.cluster'
    cluster.write_text('fixture cluster map')
    frozen = dict(seed=c.SEED, prefix_namespace=namespace, identities=A1_IDS, source_tree=source_fixture(), cluster=str(cluster.resolve()),
                  helper_hashes={name: str(i) * 64 for i, name in enumerate(('rev5-primary.py', 'rev6-production-tail.py', 'rev6-confirm.py'), 1)},
                  lifecycle_helper_sha256='4' * 64, pprof_sha256='5' * 64, source_archives={'baseline': {'sha256': '6' * 64}},
                  hashes={str((root / name).resolve()): c.sha(root / name) for name in ('schedule.json', 'schedule-diagnostics.json', 'fixture.cluster')})
    if prior is not None:
        frozen.update(prior_evidence=str(prior.resolve()), prior_frozen_sha256=c.sha(prior / 'frozen.json'))
        (root / 'inputs').mkdir()
        for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json'):
            shutil.copy2(prior / name, root / 'inputs' / ('prior-' + name))
    c.save(root / 'frozen.json', frozen)
    return frozen


class Design(unittest.TestCase):
    def test_literal_registration_counts_and_pins(self):
        self.assertEqual((spec.SEED, spec.PROCESSES, spec.EPOCH_PROCESSES, spec.AB_PER_EPOCH, spec.FORECAST), (263107, 176, 88, 40, 900))
        self.assertEqual(spec.A1_FROZEN_SHA256, '154a783499a59a5e26e3bdffbf19631b99cd2a41b67261c34556eb187493dd62')
        self.assertEqual(spec.RESULTS_SHA256, '6c700352c97e190fbc1cadf7d0a597fa6eb3f16a10bf7fb06144c9a82377deeb')
        self.assertEqual(spec.INDEX_SHA256, '6c4cd85cd82589cc11dcc6d11721d5511b0d649a59e0b26331a8cf1b0964d8c5')
        self.assertEqual(spec.PRODUCTION_BIN, '02d86dcde2f70868c116aeed225c7ef0d8a626da9b8501fb0f06a2e668c41949')
        self.assertEqual(spec.BASELINE_BIN, '2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e')
        self.assertEqual(spec.BASELINE_BIN, c.BASELINE_BIN)
        rows = c.timing_schedule(IDS, NS, spec.DESIGN)
        self.assertEqual(rows, c.timing_schedule(IDS, NS, spec.DESIGN))
        self.assertEqual(len(rows), 176)
        self.assertEqual(len({r['pair'] for r in rows}), 88)
        self.assertEqual(sum(r['iterations'] for r in rows), 3520000)
        self.assertEqual(sum(r['warmup'] for r in rows), 352000)
        self.assertEqual({r['cell'] for r in rows}, {'grv-w8'})
        for epoch in (1, 2):
            pairs = [r for r in rows if r['epoch'] == epoch and r['member'] == 0]
            self.assertEqual(len(pairs), 44)
            self.assertEqual(sum(r['contrast'] == 'AB' for r in pairs), 40)
            for order in (0, 1):
                self.assertEqual(sum(r['contrast'] == 'AB' and r['order'] == order for r in pairs), 20)
                for kind in ('baselineAA', 'productionAA'):
                    self.assertEqual(sum(r['contrast'] == kind and r['order'] == order for r in pairs), 1)
        self.assertEqual(c.timing_epoch_count(), 128)
        self.assertEqual(c.timing_count(), 256)
        self.assertEqual(c.timing_seed(), 263007)
        self.assertEqual(c.timing_forecast(), 1500)
        self.assertEqual(c.timing_count(spec.DESIGN), 176)
        self.assertEqual(c.timing_forecast(spec.DESIGN), 900)
        c.validate_schedule(c.schedule(IDS), IDS)
        with self.assertRaises(ValueError): c.validate_schedule(rows, IDS)
        with self.assertRaises(ValueError): c.validate_timing_schedule(c.schedule(IDS), IDS, spec.DESIGN)
        for function in (c.timing_epoch_count, c.timing_count, c.timing_seed, c.timing_forecast):
            with self.assertRaises(ValueError): function('unknown')

    def test_all_schedule_dimensions_reject_mutations(self):
        rows = spec.schedule(IDS, NS)
        for bad in ([], rows[:-1], rows[:-1] + [rows[0]]):
            with self.assertRaises(ValueError): spec.validate_schedule(bad, IDS)
        changes = dict(id='bad', epoch=3, visit=99, position=1, block=999, cell='get100-w8', workload='get100', workers=32,
                       iterations=500000, warmup=0, pair='bad', contrast='wrong', order=3, member=3, alias='X',
                       binary='wrong', prefix='x', profile='mutex', source='wrong', binary_sha256='wrong')
        for key, value in changes.items():
            bad = deepcopy(rows)
            bad[0][key] = value
            with self.subTest(key=key), self.assertRaises((ValueError, KeyError)): spec.validate_schedule(bad, IDS)
        for key in ('epoch', 'visit', 'position', 'block', 'workers', 'iterations', 'warmup', 'order', 'member'):
            bad = deepcopy(rows)
            bad[0][key] = True
            with self.subTest(boolean=key), self.assertRaises(ValueError): spec.validate_schedule(bad, IDS)
        for namespace in ('', 'short', 'A' * 32, None):
            with self.assertRaises(ValueError): spec.schedule(IDS, namespace)
        wrong = deepcopy(IDS)
        wrong['production']['binary_sha256'] = '2' * 64
        with self.assertRaises(ValueError): spec.schedule(wrong, NS)
        status = dict(complete=True, input_hashes_verified_after=True, completed=[r['id'] for r in rows[:88]])
        c.check_status(status, rows[:88], expected_count=88)
        with self.assertRaises(ValueError): c.check_status(status, rows[:88])
        for count in (48, 96, 176, 0, True, 88.0):
            with self.assertRaises(ValueError): c.check_status(status, rows[:88], expected_count=count)

    def test_192_prefixes_disjoint_from_all544_prior_values(self):
        timing, diagnostics = spec.schedule(IDS, NS), c.diagnostic_schedule(IDS, NS)
        original = c.schedule(A1_IDS) + c.diagnostic_schedule(A1_IDS)
        a1 = c.schedule(A1_IDS, 'a' * 32) + c.diagnostic_schedule(A1_IDS, 'a' * 32)
        spec.validate_prefix_population(timing, diagnostics, (a1, original))
        self.assertEqual(len({r['prefix'] for r in timing + diagnostics}), 192)
        self.assertEqual(len({r['prefix'] for r in a1 + original}), 544)
        for row in timing + diagnostics:
            self.assertEqual(len(row['prefix'].encode()), 68)
            self.assertEqual(len(row['prefix'].removeprefix('clientbench/rfc263/').removesuffix('/')), 48)
        for prior in (a1, original):
            bad = deepcopy(diagnostics)
            bad[0]['prefix'] = prior[0]['prefix']
            with self.assertRaises(ValueError): spec.validate_prefix_population(timing, bad, (a1, original))
        collision = deepcopy(diagnostics)
        collision[0]['prefix'] = timing[0]['prefix']
        with self.assertRaises(ValueError): spec.validate_prefix_population(timing, collision)
        with self.assertRaises(ValueError): spec.validate_prefix_population([], diagnostics)
        with self.assertRaises(ValueError): spec.validate_prefix_population(timing, diagnostics, (a1[:-1],))


class Retention(unittest.TestCase):
    def test_retained14_bound_source_and_supersession_guards(self):
        result = retained_fixture()
        index = {'results.json': spec.RESULTS_SHA256}
        spec.validate_retained(result, index, IDS, A1_IDS)
        self.assertNotEqual(IDS['production']['source'], A1_IDS['production']['source'])
        for metric in a.ENDPOINTS:
            for key, value in (('point_ratio', 1.02), ('upper_ratio', 1.0200001), ('lower_ratio', float('nan')),
                               ('confidence', .975), ('epoch_counts', [40, 40])):
                bad = deepcopy(result)
                bad['primary_and_coprimary'][metric]['get100-w8'][key] = value
                with self.subTest(metric=metric, key=key), self.assertRaises(ValueError): spec.validate_retained(bad, index, IDS, A1_IDS)
        for field in ('seed', 'processes', 'pairs', 'ab_pairs_per_cell', 'aa_pairs_per_cell', 'measured_transactions', 'warmup_transactions'):
            bad = deepcopy(result)
            bad[field] = 0
            with self.assertRaises(ValueError): spec.validate_retained(bad, index, IDS, A1_IDS)
        for bad in ({}, {'results.json': '0' * 64}):
            with self.assertRaises(ValueError): spec.validate_retained(result, bad, IDS, A1_IDS)
        bad = deepcopy(result)
        del bad['primary_and_coprimary'][a.ENDPOINTS[0]]['range100-w32']
        with self.assertRaises(ValueError): spec.validate_retained(bad, index, IDS, A1_IDS)
        wrong = deepcopy(A1_IDS)
        wrong['production']['source'] = IDS['production']['source']
        with self.assertRaises(ValueError): spec.validate_retained(result, index, IDS, wrong)
        wrong = deepcopy(IDS)
        wrong['production']['binary_sha256'] = '0' * 64
        with self.assertRaises(ValueError): spec.validate_retained(result, index, wrong)

    def test_independent_pins_reject_live_copy_and_index_races(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            live = e / 'analysis'
            results, index, rp, ip = retained_files(live)
            inputs = e / 'inputs'
            inputs.mkdir()
            shutil.copy2(results, inputs / 'retained-a1-results.json')
            shutil.copy2(index, inputs / 'retained-a1-artifact-sha256.json')
            c.save(inputs / 'prior-frozen.json', {'identities': A1_IDS})
            prior_pin = c.sha(inputs / 'prior-frozen.json')
            frozen = dict(timing_design=spec.DESIGN, identities=IDS, prior_frozen_sha256=prior_pin,
                retained_a1=dict(results_path=str(results), index_path=str(index), results_sha256=rp, index_sha256=ip))
            with patch.object(spec, 'RESULTS_SHA256', rp), patch.object(spec, 'INDEX_SHA256', ip), patch.object(spec, 'A1_FROZEN_SHA256', prior_pin):
                self.assertEqual(c.retained_a1(e, frozen), retained_fixture())
                for path in (results, index, inputs / 'retained-a1-results.json', inputs / 'retained-a1-artifact-sha256.json'):
                    original = path.read_bytes()
                    path.write_text('{}')
                    with self.subTest(path=path.name), self.assertRaises(ValueError): c.retained_a1(e, frozen)
                    path.write_bytes(original)
                with self.assertRaises(ValueError): spec.retained_results(results, index, '0' * 64, ip, IDS)
                with self.assertRaises(ValueError): spec.retained_results(results, index, rp, '0' * 64, IDS)

    def test_verify_inputs_binds_both_ancestral_populations(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            invalid, prior, e = root / 'invalid', root / 'a1', root / 'a2'
            prefix_batch(invalid, '')
            prior_meta = prefix_batch(prior, 'a' * 32, invalid)
            current_tree = deepcopy(prior_meta['source_tree'])
            current_tree['files']['tools/benchcampaign/campaigns/rfc-263/cpu_a2_spec.py'] = dict(sha256='a' * 64, mode=33188, symlink=False)
            proof = admission.source_equivalence(current_tree, prior_meta['source_tree'])
            e.mkdir()
            inputs = e / 'inputs'
            inputs.mkdir()
            for source, prefix in ((prior, 'prior-'), (invalid, 'ancestor-')):
                for name in ('frozen.json', 'schedule.json', 'schedule-diagnostics.json'):
                    shutil.copy2(source / name, inputs / (prefix + name))
            results, index, rp, ip = retained_files(root / 'analysis')
            shutil.copy2(results, inputs / 'retained-a1-results.json')
            shutil.copy2(index, inputs / 'retained-a1-artifact-sha256.json')
            c.save(e / 'schedule.json', spec.schedule(IDS, NS))
            c.save(e / 'schedule-diagnostics.json', c.diagnostic_schedule(IDS, NS))
            c.save(inputs / 'source-equivalence.json', proof)
            marker = root / 'marker'
            marker.write_text('frozen fixture input')
            frozen = dict(timing_design=spec.DESIGN, seed=spec.SEED, identities=IDS, prefix_namespace=NS,
                source_root=str(root / 'source'), source_tree=current_tree, source_equivalence=proof,
                helper_hashes=prior_meta['helper_hashes'], lifecycle_helper_sha256=prior_meta['lifecycle_helper_sha256'],
                pprof_sha256=prior_meta['pprof_sha256'], cluster=prior_meta['cluster'], source_archives=prior_meta['source_archives'],
                hashes={str(marker): c.sha(marker), prior_meta['cluster']: c.sha(prior_meta['cluster'])},
                prior_evidence=str(prior.resolve()), prior_frozen_sha256=c.sha(prior / 'frozen.json'),
                ancestor_evidence=str(invalid.resolve()), ancestor_frozen_sha256=c.sha(invalid / 'frozen.json'),
                retained_a1=dict(results_path=str(results), index_path=str(index), results_sha256=rp, index_sha256=ip))
            with patch.object(spec, 'RESULTS_SHA256', rp), patch.object(spec, 'INDEX_SHA256', ip), \
                 patch.object(spec, 'A1_FROZEN_SHA256', frozen['prior_frozen_sha256']), patch.object(c, 'source_tree', return_value=frozen['source_tree']):
                c.verify_inputs(e, frozen)
                paths = [inputs / 'ancestor-schedule.json', invalid / 'schedule.json', prior / 'schedule.json',
                         prior / 'inputs' / 'prior-frozen.json', results, inputs / 'retained-a1-results.json', inputs / 'source-equivalence.json']
                for path in paths:
                    raw = path.read_bytes()
                    path.write_text('{}')
                    with self.subTest(path=path), self.assertRaises((ValueError, KeyError)): c.verify_inputs(e, frozen)
                    path.write_bytes(raw)
                bad = dict(frozen, ancestor_frozen_sha256='0' * 64)
                with self.assertRaises(ValueError): c.verify_inputs(e, bad)


class Admission(unittest.TestCase):
    def test_actual_a1_manifest_and_source_equivalence_allowlist(self):
        with self.assertRaises(ValueError): c.a2_ancestry(Path('/must-not-be-read'), '0' * 64)
        prior = source_fixture()
        current = deepcopy(prior)
        for path in ('tools/benchcampaign/campaigns/rfc-263/cpu_a2_spec.py', 'docs/benchmarks/rfc-263/confirmation.md', 'rfcs/263-update.md', 'TODO.md', 'pr-body.md'):
            current['files'][path] = dict(sha256='a' * 64, mode=33188, symlink=False)
        proof = admission.source_equivalence(current, prior)
        self.assertEqual((proof['prior_files'], proof['current_files'], len(proof['changed_paths'])), (1001, 1006, 5))
        self.assertTrue(proof['external_configs_identical'])
        for path in ('pkg/fixture0.go', 'cmd/tool/main.go', 'tools/clientbench/run.py', 'BUILD.bazel', 'MODULE.bazel', 'go.mod', '.bazelrc', 'user.bazelrc', 'tools/benchcampaign/campaigns/rfc-263/lifecycle_helper.py', 'docs/benchmarks/rfc-263/bad.go', 'docs/benchmarks/rfc-263/BUILD.bazel'):
            bad = deepcopy(current)
            bad['files'][path] = dict(sha256='0' * 64, mode=33188, symlink=False)
            with self.subTest(path=path), self.assertRaises(ValueError): admission.source_equivalence(bad, prior)
        for path in ('/absolute', '../TODO.md', 'rfcs/sub/263-update.md', 'tools/benchcampaign/campaigns/rfc-263/../cpu_collect.py'):
            self.assertFalse(admission.allowed_change(path))
        with self.assertRaises(ValueError): admission.source_equivalence(prior, prior)
        bad = deepcopy(current)
        bad['external_configs']['fixture-user.bazelrc'] = '0' * 64
        with self.assertRaises(ValueError): admission.source_equivalence(bad, prior)
        bad['external_configs'] = {}
        with self.assertRaises(ValueError): admission.source_equivalence(bad, prior)
        bad = deepcopy(current)
        bad['files'] = {}
        with self.assertRaises(ValueError): admission.source_equivalence(bad, prior)

    def test_every_historical_input_pin_matches_the_actual_a1_contract(self):
        prior = dict(helper_hashes={name: str(i) * 64 for i, name in enumerate(('primary', 'tail', 'confirm'), 1)},
            lifecycle_helper_sha256='4' * 64, pprof_sha256='5' * 64, cluster='/fixture/a1.cluster',
            hashes={'/fixture/a1.cluster': '6' * 64}, source_archives={'baseline': {'sha256': '7' * 64}})
        current = admission.historical_pins(prior)
        admission.historical_inputs(current, prior)
        for name in current:
            bad = deepcopy(current)
            bad[name] = {} if name == 'helper_hashes' else '0' * 64
            with self.subTest(pin=name), self.assertRaises(ValueError): admission.historical_inputs(bad, prior)
        for helper in current['helper_hashes']:
            bad = deepcopy(current)
            bad['helper_hashes'][helper] = '0' * 64
            with self.subTest(helper=helper), self.assertRaises(ValueError): admission.historical_inputs(bad, prior)
        copied = deepcopy(prior)
        copied['cluster'] = '/fixture/copied.cluster'
        copied['hashes'] = {'/fixture/copied.cluster': '6' * 64}
        admission.historical_inputs(admission.historical_pins(copied), prior)
        with self.assertRaises(ValueError): admission.historical_inputs({}, prior)

    def test_confirmation_rechecks_both_numeric_targets_retained14_and_bindings(self):
        result = confirmation_fixture()
        frozen = {'identities': IDS}
        seals = result['epoch_seals']
        admission.confirmation(result, frozen, 'f' * 64, seals, retained_fixture())
        changes = dict(timing_design='a1', seed=263007, processes=256, pairs=128, fresh_ab_pairs=12, fresh_aa_pairs=4,
            measured_transactions=0, warmup_transactions=0, target_cell='get100-w8', target_ab_pairs=12,
            target_ab_pairs_per_epoch=[6, 6], aa_pairs_per_role=2, frozen_sha256='0' * 64, epoch_seals={}, identities={})
        for key, value in changes.items():
            bad = deepcopy(result)
            bad[key] = value
            with self.subTest(field=key), self.assertRaises((ValueError, KeyError)): admission.confirmation(bad, frozen, 'f' * 64, seals, retained_fixture())
        for key in ('target_decision', 'decision'):
            bad = deepcopy(result)
            bad[key]['verdict'] = 'UNRESOLVED'
            with self.assertRaises(ValueError): admission.confirmation(bad, frozen, 'f' * 64, seals, retained_fixture())
        for metric in a.ENDPOINTS:
            for key, value in (('point_ratio', 1.02), ('upper_ratio', 1.020001), ('lower_ratio', float('nan')), ('epoch_counts', [6, 6])):
                bad = deepcopy(result)
                bad['target_primary_and_coprimary'][metric][key] = value
                with self.subTest(metric=metric, field=key), self.assertRaises(ValueError): admission.confirmation(bad, frozen, 'f' * 64, seals, retained_fixture())
            bad = deepcopy(result)
            bad['primary_and_coprimary'][metric]['get100-w8']['point_ratio'] = .999
            with self.assertRaises(ValueError): admission.confirmation(bad, frozen, 'f' * 64, seals, retained_fixture())
            bad = deepcopy(result)
            bad['primary_and_coprimary'][metric][spec.TARGET]['upper_ratio'] = 1.005
            with self.assertRaises(ValueError): admission.confirmation(bad, frozen, 'f' * 64, seals, retained_fixture())
        bad = deepcopy(result)
        bad['retained_a1']['results_sha256'] = '0' * 64
        with self.assertRaises(ValueError): admission.confirmation(bad, frozen, 'f' * 64, seals, retained_fixture())

    def test_derived_confirmation_copy_recheck_and_sealed_admission(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, e = Path(tmp), Path(tmp) / 'evidence'
            e.mkdir()
            directory = e / 'diagnostics'
            directory.mkdir()
            frozen = dict(timing_design=spec.DESIGN, identities=IDS)
            c.save(e / 'frozen.json', frozen)
            frozen_pin = c.sha(e / 'frozen.json')
            seals = {'1': '1' * 64, '2': '2' * 64}
            result = confirmation_fixture(frozen_pin, seals)
            live = root / 'confirmation.json'
            c.save(live, result)
            args = SimpleNamespace(confirmation_results=live, confirmation_results_sha256=c.sha(live), frozen_sha256=frozen_pin)
            with patch.object(c, 'confirmation_epoch_seals', return_value=seals), patch.object(c, 'retained_a1', return_value=retained_fixture()):
                c.admit_a2_confirmation(args, e, frozen, directory)
                c.recheck_a2_confirmation(args, e, frozen, directory)
                c.save(directory / 'artifact-sha256.json', {p.name: c.sha(p) for p in directory.iterdir() if p.is_file()})
                c.verify_a2_confirmation_admission(e, frozen, frozen_pin, seals, c.sha(directory / 'artifact-sha256.json'))
                raw = live.read_bytes()
                live.write_text('{}')
                with self.assertRaises(ValueError): c.recheck_a2_confirmation(args, e, frozen, directory)
                live.write_bytes(raw)
                copied = directory / 'confirmation-results.json'
                raw = copied.read_bytes()
                copied.write_text('{}')
                with self.assertRaises(ValueError): c.recheck_a2_confirmation(args, e, frozen, directory)
                copied.write_bytes(raw)
                with self.assertRaises(ValueError): c.verify_a2_confirmation_admission(e, frozen, '0' * 64, seals, c.sha(directory / 'artifact-sha256.json'))
            other = e / 'copy-race'
            other.mkdir()
            def raced_copy(source, destination): Path(destination).write_text('{}')
            with patch.object(c, 'confirmation_epoch_seals', return_value=seals), patch.object(c, 'retained_a1', return_value=retained_fixture()), \
                 patch.object(c.shutil, 'copy2', side_effect=raced_copy):
                with self.assertRaises(ValueError): c.admit_a2_confirmation(args, e, frozen, other)
            self.assertFalse((other / 'confirmation-admission.json').exists())

    def test_confirmation_epoch_guard_requires_both_full88_populations(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            (e / 'inputs').mkdir()
            guard = e / 'inputs' / 'cpu_analyze.py'
            guard.write_text('not imported by this fixture')
            rows = spec.schedule(IDS, NS)
            c.save(e / 'schedule.json', rows)
            for epoch in (1, 2):
                (e / f'epoch{epoch}').mkdir()
                c.save(e / f'epoch{epoch}' / 'artifact-sha256.json', {'fixture': 'x'})
            frozen = dict(identities=IDS, hashes={str(guard.resolve()): c.sha(guard)})
            checked = SimpleNamespace(checked_epoch=Mock())
            with patch.object(c, 'load_module', return_value=checked): c.confirmation_epoch_seals(e, frozen)
            self.assertEqual([len(call.args[3]) for call in checked.checked_epoch.call_args_list], [88, 88])
            self.assertTrue(all(call.kwargs['expected_count'] == 88 for call in checked.checked_epoch.call_args_list))
            checked.checked_epoch.side_effect = [None, ValueError('epoch2 incomplete')]
            with patch.object(c, 'load_module', return_value=checked), self.assertRaises(ValueError): c.confirmation_epoch_seals(e, frozen)


class StatisticsAndDecision(unittest.TestCase):
    def test_power_projection_perfect_correlation_and_negative_pilot_guards(self):
        power = c.load_module(Path(__file__).with_name('cpu_a2_power.py'), 'cpu_a2_power_fixture')
        pairs = [dict(cell=spec.TARGET, contrast='AB', pair=f'e{epoch}-p{i}', epoch=epoch,
                      logs={a.ENDPOINTS[0]: value, a.ENDPOINTS[1]: 2 * value})
                 for epoch in (1, 2) for i, value in enumerate([-.1, .1] * 3)]
        def results_for(rows):
            return dict(primary_and_coprimary={metric: {spec.TARGET: dict(point_ratio=1,
                epoch_variances_log=[statistics.variance(p['logs'][metric] for p in rows if p['epoch'] == epoch) for epoch in (1, 2)])}
                for metric in a.ENDPOINTS})
        pilot = results_for(pairs)
        self.assertAlmostEqual(pilot['primary_and_coprimary'][a.ENDPOINTS[0]][spec.TARGET]['epoch_variances_log'][0], .012)
        self.assertAlmostEqual(pilot['primary_and_coprimary'][a.ENDPOINTS[1]][spec.TARGET]['epoch_variances_log'][0], .048)
        report = power.project(pilot, pairs)
        self.assertEqual((report['pairs_per_epoch'], report['total_AB_pairs']), (40, 80))
        self.assertEqual(report['within_epoch_pooled_correlation'], 1)
        self.assertAlmostEqual(report['endpoints'][a.ENDPOINTS[0]]['se_log'], math.sqrt(3 / 20000))
        self.assertAlmostEqual(report['endpoints'][a.ENDPOINTS[0]]['df'], 78)
        self.assertEqual(report['joint_normal_plugin_power_true_1'], min(r['normal_plugin_power_true_1'] for r in report['endpoints'].values()))
        opposite = deepcopy(pairs)
        for row in opposite: row['logs'][a.ENDPOINTS[1]] *= -1
        anti = power.project(results_for(opposite), opposite)
        self.assertEqual(anti['within_epoch_pooled_correlation'], -1)
        self.assertAlmostEqual(anti['joint_normal_plugin_power_true_1'], max(0, sum(r['normal_plugin_power_true_1'] for r in anti['endpoints'].values()) - 1))
        for bad in (pairs[:-1], pairs[:-1] + [pairs[0]]):
            with self.assertRaises(ValueError): power.project(pilot, bad)
        bad = deepcopy(pairs)
        bad[0]['logs'][a.ENDPOINTS[0]] = float('nan')
        with self.assertRaises(ValueError): power.project(pilot, bad)
        wrong = deepcopy(pilot)
        wrong['primary_and_coprimary'][a.ENDPOINTS[0]][spec.TARGET]['epoch_variances_log'][0] = .1
        with self.assertRaises(ValueError): power.project(wrong, pairs)
        zero = deepcopy(pairs)
        for row in zero: row['logs'] = {metric: 0 for metric in a.ENDPOINTS}
        with self.assertRaises(ValueError): power.project(results_for(zero), zero)

    def test_power_cli_uses_verified_buffers_across_decode_read_race(self):
        power = c.load_module(Path(__file__).with_name('cpu_a2_power.py'), 'cpu_a2_power_io_fixture')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            results, pairs, index = root / 'results.json', root / 'pair-rows.json', root / 'artifact-sha256.json'
            original_result, original_pairs = {'marker': 'original'}, [{'pilot': 'original'}]
            c.save(results, original_result)
            c.save(pairs, original_pairs)
            result_pin = c.sha(results)
            c.save(index, {'results.json': result_pin, 'pair-rows.json': c.sha(pairs)})
            original_bytes, loads = results.read_bytes(), power.json.loads
            calls = [0]
            def decode(data):
                calls[0] += 1
                if calls[0] == 1: results.write_text('{"marker":"modified-between-hash-and-decode"}')
                if calls[0] == 2: results.write_bytes(original_bytes)
                return loads(data)
            argv = ['cpu_a2_power.py', '--results', str(results), '--pairs', str(pairs), '--index', str(index), '--lock', str(root / 'lane.lock')]
            with patch.object(power, 'RESULTS_SHA256', result_pin), patch.object(power, 'INDEX_SHA256', c.sha(index)), \
                 patch.object(power.os, 'getpriority', return_value=19), patch.object(power.os, 'sched_getaffinity', return_value=power.CPUS), \
                 patch.object(sys, 'argv', argv), patch.object(power.json, 'loads', side_effect=decode), \
                 patch.object(power, 'project', return_value={}) as projected, patch.dict(sys.modules, {'scipy': SimpleNamespace(__version__='fixture')}), \
                 patch('builtins.print') as output:
                power.main()
            projected.assert_called_once_with(original_result, original_pairs)
            output.assert_called_once()
            self.assertEqual(results.read_bytes(), original_bytes)

    def test_power_cli_rejects_project_time_drift_without_output(self):
        power = c.load_module(Path(__file__).with_name('cpu_a2_power.py'), 'cpu_a2_power_drift_fixture')
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            results, pairs, index = root / 'results.json', root / 'pair-rows.json', root / 'artifact-sha256.json'
            c.save(results, {'marker': 'original'})
            c.save(pairs, [{'pilot': 'original'}])
            result_pin = c.sha(results)
            c.save(index, {'results.json': result_pin, 'pair-rows.json': c.sha(pairs)})
            def changed(result, pair_rows):
                results.write_text('{"marker":"modified-during-project"}')
                return {}
            argv = ['cpu_a2_power.py', '--results', str(results), '--pairs', str(pairs), '--index', str(index), '--lock', str(root / 'lane.lock')]
            with patch.object(power, 'RESULTS_SHA256', result_pin), patch.object(power, 'INDEX_SHA256', c.sha(index)), \
                 patch.object(power.os, 'getpriority', return_value=19), patch.object(power.os, 'sched_getaffinity', return_value=power.CPUS), \
                 patch.object(sys, 'argv', argv), patch.object(power, 'project', side_effect=changed), patch('builtins.print') as output:
                with self.assertRaisesRegex(ValueError, 'planning input drift'): power.main()
            output.assert_not_called()

    def test_n40_welch_literal_values_and_no_arbitrary_n(self):
        # Per epoch alternating +/-0.1 has variance0.4/39. Doubling the
        # second epoch gives variance1.6/39: SE=sqrt(1/3120), df=975/17.
        first = [-.1, .1] * 20
        result = a.interval([first, [2 * x for x in first]], n=40)
        self.assertAlmostEqual(result['mean_log'], 0)
        self.assertAlmostEqual(result['se_log'], (1 / 3120) ** .5)
        self.assertAlmostEqual(result['df'], 975 / 17)
        self.assertEqual(result['epoch_counts'], [40, 40])
        constant = a.interval([[0] * 40, [0] * 40], n=40)
        self.assertEqual((constant['point_ratio'], constant['lower_ratio'], constant['upper_ratio']), (1, 1, 1))
        for n in (20, 39, 41, True, 40.0):
            with self.assertRaises(ValueError): a.interval([first, first], n=n)
        for values in ([first[:-1], first], [first, []], [[float('nan')] * 40, first]):
            with self.assertRaises(ValueError): a.interval(values, n=40)
        with self.assertRaises(ValueError): a.interval([first, first])  # A1 default stays n6.

    def test_target_exact_boundaries_both_endpoints_and_old_target_not_pooled(self):
        retained = retained_fixture()
        target = {metric: dict(point_ratio=1, lower_ratio=.99, upper_ratio=1.02, confidence=.95, epoch_counts=[40, 40]) for metric in a.ENDPOINTS}
        combined, decision = subject.combine_reports(retained, target)
        self.assertEqual(decision['verdict'], 'PASS')
        self.assertFalse(decision['park'])
        for metric in a.ENDPOINTS:
            self.assertEqual(combined[metric][spec.TARGET]['point_ratio'], 1)
            self.assertEqual(retained['primary_and_coprimary'][metric][spec.TARGET]['point_ratio'], 1.2)
            bad = deepcopy(target)
            bad[metric]['upper_ratio'] = 1.0200001
            self.assertEqual(subject.combine_reports(retained, bad)[1]['verdict'], 'UNRESOLVED')
            self.assertTrue(subject.combine_reports(retained, bad)[1]['park'])
            bad[metric].update(point_ratio=1.02, upper_ratio=1.03)
            self.assertEqual(subject.combine_reports(retained, bad)[1]['verdict'], 'FAIL')
            bad[metric].update(point_ratio=.98, lower_ratio=.97, upper_ratio=.999)
            self.assertTrue(subject.combine_reports(retained, bad)[1]['improvement'][metric][spec.TARGET])
            bad[metric]['upper_ratio'] = 1
            self.assertFalse(subject.combine_reports(retained, bad)[1]['improvement'][metric][spec.TARGET])
        with self.assertRaises(ValueError): subject.combine_reports(retained, {})
        bad = deepcopy(target)
        bad[a.ENDPOINTS[0]]['epoch_counts'] = [6, 6]
        with self.assertRaises(ValueError): subject.combine_reports(retained, bad)


class CollectionAndAnalysis(unittest.TestCase):
    def test_a2_supervisor_uses15minute_admission_forecast(self):
        class BeforeLaunch(Exception): pass
        with tempfile.TemporaryDirectory() as tmp:
            args = SimpleNamespace(evidence=Path(tmp), epoch=1, timing_design=spec.DESIGN, deadline_utc='fixture')
            with patch.object(c, 'deadline_seconds', return_value=905), patch.object(c, 'frozen_inputs') as inputs, self.assertRaises(ValueError):
                c.supervise(args)
            inputs.assert_not_called()
            with patch.object(c, 'deadline_seconds', return_value=910), patch.object(c, 'frozen_inputs', side_effect=BeforeLaunch), self.assertRaises(BeforeLaunch):
                c.supervise(args)

    def test_mocked_collector_requires_both88_epochs_for_future_diagnostics(self):
        for diagnostics in (False, True):
            with tempfile.TemporaryDirectory() as tmp:
                e = Path(tmp)
                rows, diag = spec.schedule(IDS, NS), c.diagnostic_schedule(IDS, NS)
                c.save(e / 'schedule.json', rows)
                c.save(e / 'schedule-diagnostics.json', diag)
                c.save(e / 'schedule-epoch1.json', rows[:88])
                if diagnostics:
                    for epoch in (1, 2):
                        (e / f'epoch{epoch}').mkdir()
                        c.save(e / f'epoch{epoch}' / 'status.json', dict(complete=True, input_hashes_verified_after=True,
                            completed=[r['id'] for r in rows if r['epoch'] == epoch]))
                frozen = dict(timing_design=spec.DESIGN, seed=spec.SEED, identities=IDS, prefix_namespace=NS,
                              cluster='fixture.cluster', lock=str(e / 'lane.lock'), helper_hashes={})
                args = SimpleNamespace(evidence=e, epoch=0 if diagnostics else 1, population='diagnostics' if diagnostics else 'timing',
                                       authorization='fixture owner approval', deadline_utc='fixture', deadline=1600)
                clock = [1.0]
                selected = diag if diagnostics else rows[:88]
                def sleep(seconds): clock[0] += seconds
                def run(command, **kwargs):
                    clock[0] += 1
                    out = Path(command[command.index('--output') + 1])
                    row = next(r for r in selected if r['id'] == out.name)
                    out.mkdir()
                    manifest, summary = base.fixture(row, out, frozen)
                    c.save(out / 'manifest.json', manifest)
                    c.save(out / 'summary.json', summary)
                    (out / 'samples.csv').write_text('fixture')
                    (out / 'warmup.csv').write_text('fixture')
                    if diagnostics: (out / (row['profile'] + '.pprof')).write_bytes(b'fixture profile')
                    return SimpleNamespace(returncode=0)
                with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                     patch.dict(c.os.environ, {}, clear=True), patch.object(c, 'frozen_inputs', return_value=frozen), \
                     patch.object(c, 'helper_chain', return_value=SimpleNamespace(gate=lambda e: 0)), \
                     patch.object(c.time, 'sleep', side_effect=sleep), patch.object(c.time, 'monotonic', side_effect=lambda: clock[0]), \
                     patch.object(c.time, 'monotonic_ns', side_effect=lambda: int(clock[0] * 1e9)), patch.object(c.subprocess, 'run', side_effect=run) as child, \
                     patch.object(c, 'csv_integrity') as raw, patch.object(c, 'validate_lifecycle'), patch.object(c, 'verify_inputs'), \
                     patch.object(c, 'admit_a2_confirmation') as admitted, patch.object(c, 'recheck_a2_confirmation') as rechecked, patch('builtins.print'):
                    c.collect(args)
                    self.assertEqual(child.call_count, 16 if diagnostics else 88)
                    self.assertEqual(raw.call_count, 32 if diagnostics else 176)
                    self.assertEqual(admitted.call_count, 1 if diagnostics else 0)
                    self.assertEqual(rechecked.call_count, 1 if diagnostics else 0)
                population = e / ('diagnostics' if diagnostics else 'epoch1')
                self.assertTrue(c.read_json(population / 'status.json')['complete'])
                self.assertFalse((population / 'artifact-sha256.json').exists())

    def test_a2_diagnostics_reject_a1_sized_prior_status_before_launch(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp)
            rows, diag = spec.schedule(IDS, NS), c.diagnostic_schedule(IDS, NS)
            c.save(e / 'schedule.json', rows)
            c.save(e / 'schedule-diagnostics.json', diag)
            for epoch in (1, 2):
                (e / f'epoch{epoch}').mkdir()
                c.save(e / f'epoch{epoch}' / 'status.json', dict(complete=True, input_hashes_verified_after=True,
                    completed=[r['id'] for r in (c.schedule(IDS)[:128] if epoch == 1 else rows[88:])]))
            frozen = dict(timing_design=spec.DESIGN, seed=spec.SEED, identities=IDS, prefix_namespace=NS, helper_hashes={})
            args = SimpleNamespace(evidence=e, epoch=0, population='diagnostics', authorization='fixture', deadline_utc='fixture')
            with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.dict(c.os.environ, {}, clear=True), patch.object(c, 'frozen_inputs', return_value=frozen), \
                 patch.object(c, 'helper_chain'), patch.object(c.subprocess, 'run') as child:
                with self.assertRaises(ValueError): c.collect(args)
                child.assert_not_called()

    def evidence(self, e):
        rows = spec.schedule(IDS, NS)
        frozen = dict(timing_design=spec.DESIGN, seed=spec.SEED, identities=IDS, prefix_namespace=NS, source_root=str(e.parent / 'source'),
                      lock=str(e.parent / 'lane.lock'), cluster='fixture.cluster', helper_hashes={})
        c.save(e / 'frozen.json', frozen)
        c.save(e / 'schedule.json', rows)
        seals = []
        for epoch in (1, 2):
            directory = e / f'epoch{epoch}'
            (directory / 'runs').mkdir(parents=True)
            selected = [r for r in rows if r['epoch'] == epoch]
            c.save(directory / 'status.json', dict(epoch=epoch, complete=True, input_hashes_verified_after=True, completed=[r['id'] for r in selected]))
            for row in selected:
                out = directory / 'runs' / row['id']
                out.mkdir()
                manifest, summary = base.fixture(row, out, frozen, .99 if row['binary'] == 'production' else 1)
                c.save(out / 'manifest.json', manifest)
                c.save(out / 'summary.json', summary)
                for name in ('samples.csv', 'warmup.csv'): (out / name).write_text('fixture')
                c.save(directory / 'runs' / (row['id'] + '.command.json'), dict(schedule=row, returncode=0, env=c.ENV, timeout_seconds=120, duration_ns=1))
            c.save(directory / 'artifact-sha256.json', {str(p.relative_to(directory)): c.sha(p) for p in directory.rglob('*') if p.is_file()})
            seals.append(c.sha(directory / 'artifact-sha256.json'))
        return frozen, seals

    def test_full_a2_analyzer_and_incomplete_epoch_blocks_all_fresh_estimates(self):
        with tempfile.TemporaryDirectory() as tmp:
            e = Path(tmp) / 'evidence'
            e.mkdir()
            frozen, seals = self.evidence(e)
            args = SimpleNamespace(evidence=e, output=Path(tmp) / 'analysis', frozen_sha256=c.sha(e / 'frozen.json'), seal1=seals[0], seal2=seals[1])
            with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.object(c, 'verify_inputs'), patch.object(c, 'retained_a1', return_value=retained_fixture()), patch.object(c, 'helper_chain'), \
                 patch.object(c, 'validate_lifecycle'), patch.object(c, 'csv_integrity') as raw, patch.object(c, 'durable', return_value=True), patch('builtins.print'):
                subject.analyze(args)
            result = c.read_json(args.output / 'results.json')
            self.assertEqual((result['processes'], result['pairs'], result['fresh_ab_pairs'], result['fresh_aa_pairs']), (176, 88, 80, 8))
            self.assertEqual(result['decision']['verdict'], 'PASS')
            self.assertEqual(raw.call_count, 352)
            for metric in a.ENDPOINTS:
                self.assertAlmostEqual(result['primary_and_coprimary'][metric][spec.TARGET]['point_ratio'], .99)
                self.assertEqual(result['primary_and_coprimary'][metric][spec.TARGET]['epoch_counts'], [40, 40])
                self.assertEqual(result['aa_diagnostics'][metric]['baselineAA']['epoch_pair_counts'], [2, 2])
                self.assertEqual(result['aa_diagnostics'][metric]['baselineAA']['pairs'], 4)
                self.assertIsNone(result['aa_diagnostics'][metric]['baselineAA']['interval'])
                for cell in a.CELL_NAMES - {spec.TARGET}:
                    self.assertEqual(result['primary_and_coprimary'][metric][cell], retained_fixture()['primary_and_coprimary'][metric][cell])
            self.assertEqual(result['ab_pairs_by_cell'][spec.TARGET], 80)
            self.assertEqual(result['ab_pairs_by_cell']['get100-w8'], 12)
            directory = e / 'epoch2'
            status = c.read_json(directory / 'status.json')
            status['complete'] = False
            c.save(directory / 'status.json', status)
            index = c.read_json(directory / 'artifact-sha256.json')
            index['status.json'] = c.sha(directory / 'status.json')
            c.save(directory / 'artifact-sha256.json', index)
            args.seal2 = c.sha(directory / 'artifact-sha256.json')
            with patch.object(c.os, 'getpriority', return_value=19), patch.object(c.os, 'sched_getaffinity', return_value=c.CPUS), \
                 patch.object(c, 'verify_inputs'), patch.object(subject, 'extract') as extract:
                with self.assertRaises(ValueError): subject.analyze(args)
                extract.assert_not_called()

    def test_queuewait_dispatch_checks176_before_profile_decoding(self):
        q = c.load_module(Path(__file__).with_name('rev6-cpu-queuewait.py'), 'cpu_a2_queuewait_fixture')
        class BeforeProfiles(Exception): pass
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            pprof, archive = root / 'pprof', root / 'source.tar.gz'
            pprof.write_text('not executable')
            archive.write_text('not a tar; decoder must not be reached')
            helper, analyzer = Path(c.__file__).resolve(), Path(q.__file__).resolve()
            frozen = dict(timing_design=spec.DESIGN, seed=spec.SEED, identities=IDS, prefix_namespace=NS, helper_hashes={},
                pprof_sha256=c.sha(pprof), hashes={str(p): c.sha(p) for p in (pprof, archive, helper, analyzer)},
                source_archives={role: dict(path=str(archive), sha256=c.sha(archive), source=IDS[role]['source']) for role in IDS})
            c.save(root / 'frozen.json', frozen)
            c.save(root / 'schedule.json', spec.schedule(IDS, NS))
            c.save(root / 'schedule-diagnostics.json', c.diagnostic_schedule(IDS, NS))
            args = SimpleNamespace(evidence=root, output=root / 'analysis', frozen_sha256=c.sha(root / 'frozen.json'), collector_helper=helper,
                pprof_binary=pprof, pprof_sha256=c.sha(pprof), seal1='1' * 64, seal2='2' * 64, seal='3' * 64)
            def checked(evidence, directory, seal, rows, collector):
                diagnostic = directory.name == 'diagnostics'
                return [dict(started_utc='2026-01-01T00:00:02+00:00' if diagnostic else '2026-01-01T00:00:00+00:00',
                    finished_utc='2026-01-01T00:00:03+00:00' if diagnostic else '2026-01-01T00:00:01+00:00') for _ in rows]
            with patch.object(q, 'load_module', return_value=c), patch.object(c, 'verify_inputs'), patch.object(c, 'helper_chain'), \
                 patch.object(q, 'checked_population', side_effect=checked) as populations, \
                 patch.object(q, 'require_posttiming', wraps=q.require_posttiming) as chronology, \
                 patch.object(c, 'verify_a2_confirmation_admission', return_value={'results_sha256': 'f' * 64}) as admitted, \
                 patch.object(q.SourceBinding, 'archive', side_effect=BeforeProfiles):
                with self.assertRaises(BeforeProfiles): q.analyze(args)
            self.assertEqual([len(call.args[3]) for call in populations.call_args_list], [88, 88, 16])
            self.assertEqual(chronology.call_args.args[2], 176)
            admitted.assert_called_once()
            timing = checked(root, root / 'epoch1', '', [0] * 176, c)
            diagnostics = checked(root, root / 'diagnostics', '', [0] * 16, c)
            q.require_posttiming(timing, diagnostics, expected_timing=176)
            with self.assertRaises(ValueError): q.require_posttiming(timing, diagnostics)
            for count in (96, 128, 175, 177, True, 176.0):
                with self.assertRaises(ValueError): q.require_posttiming(timing, diagnostics, expected_timing=count)


def run_tests():
    suite = unittest.defaultTestLoader.loadTestsFromModule(sys.modules[__name__])
    if suite.countTestCases() == 0:
        raise RuntimeError('empty A2 fixture population')
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    if not result.wasSuccessful():
        raise AssertionError('A2 fixture guards failed')
    return result.testsRun


if __name__ == '__main__':
    run_tests()
