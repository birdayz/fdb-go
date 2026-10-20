#!/usr/bin/env python3
"""Controller-only recovery of the frozen runner's aggregate case-label defect.

--input D/results/B --working D/results-reconstructed/B --output D/analysis/B
No measurement/code/original CSV is changed. All data except CSV case labels
must agree exactly with pinned plan identities and strict raw-log parsing.
"""
import argparse
import csv
import json
from pathlib import Path
import re
import shutil
import sys
import tempfile
import unittest

import bench_runner as runner
import bench_analyze as analyzer


def inventory(folder):
    return {str(p.relative_to(folder)): runner.sha(p) for p in sorted(folder.rglob('*')) if p.is_file()}


def check_old_csv(path, samples):
    original = analyzer.load_samples(path)
    if len(original) != len(samples):
        raise ValueError('original CSV sample count differs from raw population')
    changed = 0
    for old, recovered in zip(original, samples):
        expected = {k: recovered.get(k) for k in old}
        if old['case'] not in ('process_system_cpu_seconds', recovered['case']):
            raise ValueError('unexpected aggregate case corruption; not this known defect')
        changed += old['case'] != recovered['case']
        old = dict(old, case=recovered['case'])
        if old != expected:
            raise ValueError('original CSV differs from raw metrics beyond the known case-label defect')
    return changed


def reconstruct(source, working, output, d1_complete_case_only=False):
    source, working, output = map(lambda p: Path(p).resolve(), (source, working, output))
    if working.exists() or output.exists():
        raise FileExistsError('refusing existing reconstruction or analysis output')
    if source == working or source in working.parents or source == output or source in output.parents:
        raise ValueError('analysis/copy must not be inside original raw phase')
    originals = inventory(source)
    if not d1_complete_case_only and any((source/name).exists() for name in ('ABORTED.json', 'INCOMPLETE.json', 'ADAPTER_ADVERSE.json')):
        raise ValueError('adverse/incomplete phase is not eligible for clean reconstruction')
    plan = json.loads((source/'plan.json').read_text())
    complete = json.loads((source/'complete.json').read_text()) if (source/'complete.json').exists() else None
    derived_omitted, parser_failures = {}, []
    if d1_complete_case_only:
        aborted = json.loads((source/'ABORTED.json').read_text())
        if plan['phase']['id'] != 'D1' or aborted['error'] != "CorrectnessFailure('SQL physical prefix differs between metrics and audits')":
            raise ValueError('partial recovery is authorized only for the retained D1 calibration-prefix parser defect')
        derived_omitted = {
            'FDB_IndexScanRange': dict(reason='N/A: calibration-versus-measured directory-prefix parser defect, not production SQL correctness failure',
                                      original_phase_error=aborted['error'], partial_runs_retained_without_ratios=True),
            'FDB_TimestampInsert': dict(reason='N/A: unexecuted after phase stopped on the calibration-prefix parser defect')}
    pins = plan['expected_sha256']
    if not pins or any(not re.fullmatch('[0-9a-f]{64}', h) for h in pins.values()):
        raise ValueError('nonempty complete SHA256 pin map required')
    pin_receipts = ['pins.before.json', 'pins.finally.json']
    if not d1_complete_case_only or (source/'pins.after.json').exists():
        pin_receipts.append('pins.after.json')
    for name in pin_receipts:
        if json.loads((source/name).read_text()) != pins:
            raise ValueError('before/after/finally input pins differ: '+name)
    for name, path_key in [('bench_runner.py', 'runner'), ('bench_analyze.py', 'analyzer')]:
        path = Path(__file__).with_name(name)
        if runner.sha(path) != pins[plan['paths'][path_key]]:
            raise ValueError('local parser/analyzer differs from measurement pins')
    if json.loads((source/'controller.json').read_text())['plan_sha256'] != runner.sha(source/'plan.json') or (complete and complete['plan_sha256'] != runner.sha(source/'plan.json')):
        raise ValueError('pinned plan SHA differs from phase/controller receipts')
    if plan['bootstrap_resamples'] != 20000 or plan['analysis_seed'] != 391706:
        raise ValueError('unexpected frozen bootstrap settings')
    if not d1_complete_case_only and json.loads((source/'omitted.json').read_text()) != {}:
        raise ValueError('omitted cases require N/A, not complete reconstruction')
    jobs = plan['jobs']
    if not jobs or len({j['id'] for j in jobs}) != len(jobs):
        raise ValueError('empty/duplicate jobs')
    if not d1_complete_case_only and complete != dict(scheduled=len(jobs), completed=len(jobs), samples=len(jobs)*3,
                        pins_unchanged=True, plan_sha256=runner.sha(source/'plan.json')):
        raise ValueError('complete receipt differs from exact job/repeat population')
    locally_verified, receipt_only = {}, {}
    for runtime_path, digest in pins.items():
        if runtime_path.startswith('/srv/fdb-bench/'):
            local = Path(__file__).parent/runtime_path.removeprefix('/srv/fdb-bench/')
            if local.is_file():
                if runner.sha(local) != digest:
                    raise ValueError('available controller artifact does not match runtime pin: '+str(local))
                locally_verified[str(local)] = digest
                continue
        # VM binaries/config/system paths are not reinterpreted as controller
        # paths. Their exact full pin maps are verified against all3 receipts.
        receipt_only[runtime_path] = digest
    samples = []
    for job in jobs:
        folder = source/job['id']
        if d1_complete_case_only and not folder.exists() and job['case']['key'] in derived_omitted:
            continue
        if d1_complete_case_only and folder.exists() and job['case']['key'] == 'FDB_TimestampInsert':
            raise ValueError('TimestampInsert was expected to be unexecuted')
        if json.loads((folder/'invocation.json').read_text()) != job:
            raise ValueError('raw invocation differs from pinned plan: '+job['id'])
        result = json.loads((folder/'result.json').read_text())
        if (result['returncode'] != 0 or result['case'] != job['case']['key'] or
                any(result[k] != job[k] for k in ('id', 'block', 'position', 'arm'))):
            raise ValueError('raw job identity/status differs from plan: '+job['id'])
        if result['status'] != 'ok':
            if not (d1_complete_case_only and job['case']['key'] == 'FDB_IndexScanRange' and
                    result['status'] == 'correctness-failed' and result.get('error') == 'SQL physical prefix differs between metrics and audits'):
                raise ValueError('unexpected failed raw job: '+job['id'])
            text = (folder/'stdout.txt').read_text()
            if not re.search(r'(?m)^PASS\s*$', text):
                raise ValueError('retained prefix-parser failure did not have Go benchmark PASS')
            receipts = [json.loads(line.split('FDB_BENCH_AUDIT ', 1)[1]) for line in text.splitlines() if 'FDB_BENCH_AUDIT ' in line]
            sequence = [a['prefix_bytes'] for a in receipts if a['case'] == job['name']]
            if sorted(set(sequence)) != [5, 6]:
                raise ValueError('retained raw log does not show the known5/6-byte calibration-prefix defect')
            parser_failures.append(dict(job=job['id'], returncode=0, go_benchmark_pass=True,
                                        audit_prefix_sequence=sequence, observed_prefix_bytes=[5, 6], source_status=result['status'],
                                        interpretation='harness/parser defect; do not infer production correctness failure'))
            continue
        parsed, audits = runner.parse_output((folder/'stdout.txt').read_text()+'\n'+(folder/'stderr.txt').read_text(), job)
        if parsed != json.loads((folder/'parsed.json').read_text()) or audits != json.loads((folder/'audits.json').read_text()):
            raise ValueError('parsed/audit artifacts differ from strict raw logs: '+job['id'])
        for row in parsed:
            samples.append(dict(phase=plan['phase']['id'], case=job['case']['key'], block=job['block'],
                                position=job['position'], arm=job['arm'], repeat=row['repeat'], N=row['N'], **row['metrics']))
    validated_csv_samples = len(samples)
    changed = check_old_csv(source/'metrics.csv', samples)
    if changed != len(samples):
        raise ValueError('recovery expected exactly the observed case-label defect on every sample')
    if d1_complete_case_only:
        if {p['job'] for p in parser_failures} != {'D1-c01-b00-p1-legacy', 'D1-c01-b00-p2-final'}:
            raise ValueError('D1 parser-failure population differs from retained source evidence')
        samples = [s for s in samples if s['case'] == 'FDB_PlanCacheHit']
        if validated_csv_samples != 57 or len(samples) != 54:
            raise ValueError('D1 expected54complete-case samples plus3retained partial-case samples')
    # Normal copy, no hardlinks: frozen analyzer may write only in the new copy.
    working.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(source, working)
    (working/'metrics.csv').rename(working/'metrics.original.csv')
    runner.write_metrics(working/'metrics.csv', samples)
    if d1_complete_case_only:
        (working/'omitted.json').rename(working/'omitted.original.json')
        runner.dump(working/'omitted.json', derived_omitted)
    # This executes the exact unmodified summarize/bootstrap and Markdown logic,
    # including another independent raw-log/CSV/pin/population validation pass.
    report = analyzer.analyze(working, output)
    if inventory(source) != originals:
        raise ValueError('original phase artifacts changed during reconstruction')
    provenance = json.loads((output/'analysis-provenance.json').read_text())
    provenance['reconstruction'] = dict(
        reason='frozen runner run() CPU-loop variable key shadowed only aggregate CSV case labels',
        changed_column='case', changed_rows=len(samples), original_csv_labels_validated=changed,
        original_csv_samples_validated=validated_csv_samples, analysis_samples=len(samples), phase=plan['phase']['id'],
        partial_phase_complete_case_only=d1_complete_case_only, derived_omitted=derived_omitted,
        retained_parser_failures=parser_failures, pin_receipts_verified=pin_receipts,
        source_directory=str(source), working_directory=str(working), measurement_rerun=False,
        postprocessor_sha256=runner.sha(__file__), original_metrics_sha256=originals['metrics.csv'],
        reconstructed_metrics_sha256=runner.sha(working/'metrics.csv'), raw_source_sha256=originals,
        locally_rehashed_pin_artifacts=locally_verified,
        runtime_only_pins_verified_by_receipts=receipt_only,
        original_phase_unchanged=True, all_other_original_csv_fields_match_raw_exactly=True)
    runner.dump(output/'analysis-provenance.json', provenance)
    with open(output/'report.md', 'a') as stream:
        stream.write('\n## Reconstruction provenance\n\nAggregate CSV case labels were reconstructed from the pinned plan and unchanged per-job raw logs. '
                     f'Exactly {len(samples)} included case labels were recovered in a separate working copy; all {validated_csv_samples} original CSV rows were checked against raw data. '
                     'No measurements were repeated and the original phase directory/metrics.csv remain unchanged. '
                     'The frozen analyzer and exact paired-block summarize/bootstrap logic were used without edits. '
                     'Full raw hashes and input-pin receipt verification are recorded in analysis-provenance.json.\n')
        if d1_complete_case_only:
            stream.write('\nD1 is an aborted phase, not a complete clean phase. Only FDB_PlanCacheHit contributes18complete jobs/54samples. '
                         'FDB_IndexScanRange is N/A: the parser compared all calibration and measured prefix receipts cartesian-wise, '
                         'and saw5/6-byte directory prefixes despite Go benchmark PASS/rc0. Both legacy and final affected raw logs are retained. '
                         'Its3previously accepted partial samples remain in metrics.original.csv/raw logs, outside all ratios. '
                         'FDB_TimestampInsert was not executed. The source correctness-failed labels are retained as raw provenance, not endorsed as production failures.\n')
    return report


class ReconstructionTests(unittest.TestCase):
    def test_known_case_only_correction(self):
        row = dict(phase='B', case='SaveRecord', block=0, position=0, arm='final', repeat=0, N=1000,
                   **{'ns/op': 1.0, 'B/op': 0.0, 'allocs/op': 0.0})
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder)/'metrics.csv'
            runner.write_metrics(path, [dict(row, case='process_system_cpu_seconds')])
            self.assertEqual(check_old_csv(path, [row]), 1)
            text = path.read_text()
            path.write_text(text.replace('1.0', '2.0'))
            with self.assertRaises(ValueError):
                check_old_csv(path, [row])
            path.write_text(text.replace('process_system_cpu_seconds', 'unknown-defect'))
            with self.assertRaises(ValueError):
                check_old_csv(path, [row])

    def test_existing_outputs_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            with self.assertRaises(FileExistsError):
                reconstruct('/nonexistent', folder, '/nonexistent-output')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--input')
    p.add_argument('--working')
    p.add_argument('--output')
    p.add_argument('--self-test', action='store_true')
    p.add_argument('--d1-complete-case-only', action='store_true', help='explicit retained D1 parser-defect complete-case recovery, not a full-phase pass')
    args = p.parse_args()
    if args.self_test:
        result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(ReconstructionTests))
        return 0 if result.wasSuccessful() else 1
    if not all((args.input, args.working, args.output)):
        p.error('--input, --working and --output required')
    report = reconstruct(args.input, args.working, args.output, args.d1_complete_case_only)
    print(json.dumps(dict(phase=report['phase'], raw_samples=report['raw_samples'], cases=len(report['cases']), comparisons=report['comparisons'])))
    return 0


if __name__ == '__main__':
    sys.exit(main())
