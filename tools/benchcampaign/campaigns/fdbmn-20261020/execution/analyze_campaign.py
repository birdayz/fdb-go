#!/usr/bin/env python3
"""Offline-only analysis adapter. Never launches benchmarks, SSH, or cloud tools."""
import argparse
import csv
import hashlib
import importlib
import json
import math
import os
from pathlib import Path
import random
import shutil
import subprocess
import sys
import tarfile

sys.dont_write_bytecode = True
DEFAULT = Path('${FDB_PERF_HOME}/campaigns/fdbmn-20261020')
PRIOR = Path('${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014')
SEED, RESAMPLES, BLOCKS = 492817, 20000, 6
CONTRASTS = (('final', 'before'), ('final', 'cgo'), ('before', 'cgo'))
ALLOCATIONS = ('go_alloc_bytes_per_op', 'go_mallocs_per_op')
CLIENT_METRICS = ('mean_ns', 'p50_ns', 'p90_ns', 'p99_ns', 'p999_ns', 'process_cpu_ns_per_op',
                  'user_cpu_ns_per_op', 'system_cpu_ns_per_op', 'drained_cpu_ns_per_op', 'shutdown_process_cpu_ns_per_op')
DESCRIPTIVE = 'Different campaign cohorts; side-by-side description only. No pooled blocks, ratio-of-ratios, or cross-campaign confidence interval.'


class Refused(ValueError):
    pass


def need(ok, message):
    if not ok:
        raise Refused(message)


def sha(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def read(path):
    return json.loads(Path(path).read_text())


def dump(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open('x') as stream:
        json.dump(value, stream, indent=2, sort_keys=True, allow_nan=False)
        stream.write('\n')


def comparison_csv(path, rows):
    if not rows:
        return
    fields = ['workload', 'case', 'metric', 'numerator', 'denominator', 'status']
    fields += sorted(set().union(*(r.keys() for r in rows)) - set(fields))
    with Path(path).open('x', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows(rows)


def contained(root, path):
    root, path = Path(root).resolve(), Path(path).resolve()
    need(path.is_relative_to(root), 'path escapes campaign: ' + str(path))
    return path


def tree_hashes(path):
    return {str(p.relative_to(path)): sha(p) for p in sorted(path.rglob('*')) if p.is_file()}


def no_symlinks(path):
    need(not path.is_symlink() and all(not p.is_symlink() for p in path.rglob('*')),
         'symlinked raw artifact refused: ' + str(path))


def r4_cases(report, phase, expected):
    need(expected and len(expected) == len(set(expected)), 'empty/duplicate expected cases')
    need(report.get('phase') == phase and report.get('reconciliation_complete') is True,
         'R4 missing complete reconciliation for ' + phase)
    cases = report.get('cases')
    need(isinstance(cases, list) and len(cases) == len(expected), 'R4 incomplete case population')
    names = [c.get('case') for c in cases]
    need(len(set(names)) == len(names) and set(names) == set(expected), 'R4 unexpected/missing cases')
    need(all(c.get('verdict') in ('clean', 'contaminated', 'unverifiable') for c in cases), 'unknown R4 verdict')
    counts = {v: sum(c['verdict'] == v for c in cases) for v in ('clean', 'contaminated', 'unverifiable')}
    summary = report.get('phase_summary', {})
    need(summary.get('cases') == len(expected) and all(summary.get(k) == v for k, v in counts.items()),
         'R4 summary/count disagreement')
    need(set(summary.get('analysis_eligible_cases', [])) == {c['case'] for c in cases if c['verdict'] == 'clean'},
         'R4 eligible-case disagreement')
    return {c['case']: c for c in cases}


def exact_runs(case, ids):
    observed = [r.get('run_id') for r in case.get('runs', [])]
    need(ids and len(ids) == len(set(ids)) and len(observed) == len(ids) and set(observed) == set(ids),
         'R4 run population differs from complete case: ' + case['case'])


def paired_rows(rows, workload):
    observed = [(r['block'], r['arm']) for r in rows]
    expected = {(b, a) for b in range(BLOCKS) for a in ('before', 'final', 'cgo')}
    need(len(observed) == len(expected) and set(observed) == expected, 'not the complete six-block three-arm population')
    need(all(r['workload'] == workload and r['errors'] == 0 for r in rows), 'wrong workload or nonzero errors')
    return {(r['block'], r['arm']): r for r in rows}


def allocation_comparisons(ca, rows, workload):
    stats = paired_rows(rows, workload)
    output = []
    for numerator, denominator in CONTRASTS:
        rng = random.Random(SEED)
        for metric in ALLOCATIONS:
            pairs = [(stats[b, numerator][metric], stats[b, denominator][metric]) for b in range(BLOCKS)]
            entry = dict(workload=workload, numerator=numerator, denominator=denominator, metric=metric,
                         ratio_gt_1_means='numerator allocates more Go heap; native C++ heap excluded')
            if any(not math.isfinite(v) or v < 0 for pair in pairs for v in pair):
                raise Refused('invalid allocation counter')
            if any(v == 0 for pair in pairs for v in pair):
                entry.update(status='undefined-zero', reason='Positive log ratio undefined; absolute zero retained')
            else:
                entry.update(ca.bootstrap_log_ratio(pairs, rng, RESAMPLES))
                entry.update(ca.dependence_sensitivity(pairs, rng, RESAMPLES))
                entry['status'] = 'ok'
            output.append(entry)
    return output


class Adapter:
    def __init__(self, campaign, protocol, output, prior, run=subprocess.run):
        self.c = Path(campaign).resolve()
        self.protocol_path = contained(self.c, protocol)
        self.protocol = read(self.protocol_path)
        self.out = contained(self.c, output)
        need(self.out.is_relative_to(self.c / 'analysis'), 'output must be below campaign/analysis/')
        need(not self.out.exists(), 'refusing analysis overwrite')
        self.prior, self.run = Path(prior).resolve(), run
        self.scripts = self.c / 'frozen/tools/clientbench/scripts'
        self.sg = self.c / 'tooling/tools/benchcampaign/singlenode'
        self.mn = self.c / 'tooling/tools/benchcampaign/multinode'
        sys.path[:0] = [str(self.scripts), str(self.sg), str(self.mn)]
        self.ca = importlib.import_module('analyze')
        self.ba = importlib.import_module('bench_analyze')
        self.br = importlib.import_module('bench_runner')
        self.mr = importlib.import_module('bench_runner_mn')
        self.sm = importlib.import_module('seal_multinode')
        for module, folder in [(self.ca, self.scripts), (self.ba, self.sg), (self.br, self.sg), (self.mr, self.mn), (self.sm, self.mn)]:
            need(Path(module.__file__).resolve().parent == folder.resolve(), 'unexpected imported analyzer/helper source')
        self.tool_paths = [self.scripts / name for name in ('analyze.py', 'run.py')]
        self.tool_paths += [self.sg / name for name in ('bench_analyze.py', 'bench_runner.py')]
        self.tool_paths += [self.mn / name for name in ('bench_runner_mn.py', 'seal_multinode.py', 'reconcile_telemetry.py', 'node_sampler.py')]
        imported = [Path(m.__file__).resolve() for m in list(sys.modules.values()) if getattr(m, '__file__', None)]
        self.tool_paths = sorted(set(self.tool_paths) | {p for p in imported if p.is_relative_to(self.c / 'tooling') or p.is_relative_to(self.scripts)})
        self.source_path = self.c / 'provenance/source-receipt.json'
        self.source = read(self.source_path)
        self.build_path = self.c / 'build-check/cleanup-compat-builds.json'
        self.build = read(self.build_path)
        self.binary_hashes = {Path(p).parts[-2] + '/' + Path(p).name: v['sha256'] for p, v in self.build['executables'].items()}
        need(self.build['production_revisions'] == self.protocol['production_revisions'] or
             self.build['production_revisions'] == {'old': self.protocol['production_revisions']['before_allocation_work_after_read_parity'],
                                                    'master': self.protocol['production_revisions']['final']}, 'build/protocol revisions differ')
        need({v['sha256'] for v in self.build['sdk'].values()} == {self.source['compiler_sha256']}, 'compiler/source receipt differs')
        need(self.source['production_revisions'] == self.protocol['production_revisions'], 'source/protocol revision disagreement')
        need(self.build['base_harness_revision'] == self.source['harness_commit'] == self.protocol['frozen_harness_revision'], 'base harness revision differs')
        compatibility = self.source['harness_compatibility']
        need(self.build['source_amendment']['combined_patch_sha256'] == compatibility['patch_sha256'] and
             self.build['source_amendment']['upstream_commit'] == compatibility['upstream_commit'], 'compatibility patch identity differs')
        need(self.build['source_amendment']['new_archive_sha256'] == compatibility['archive_sha256'], 'build/amended archive differs')
        policy = self.protocol['sample_policy']
        need(policy['analysis_seed'] == SEED and policy['bootstrap_resamples'] == RESAMPLES, 'protocol analysis seed/resamples differ')
        archive = self.c / 'frozen/cleanup-compat/client-harness.tar.gz'
        need(sha(archive) == compatibility['archive_sha256'], 'cleanup-compatible source archive changed')
        with tarfile.open(archive) as tf:
            for name in ('analyze.py', 'run.py'):
                member = 'tools/clientbench/scripts/' + name
                need(tf.extractfile(member).read() == (self.scripts / name).read_bytes(), 'not the amended frozen client scripts')
        self.provenance = dict(adapter_sha256=sha(__file__), protocol_sha256=sha(self.protocol_path),
            source_receipt_sha256=sha(self.source_path), source_receipt=self.source,
            build_receipt_sha256=sha(self.build_path), build_executable_identities=self.build['executables'],
            cleanup_archive_sha256=sha(archive), tool_sha256={str(p): sha(p) for p in self.tool_paths},
            runtime_receipts={}, seed=SEED, resamples=RESAMPLES, blocks=BLOCKS,
            interpretation='Nominal 95% whole-paired-block bootstrap; six blocks, no multiplicity/equivalence/capacity claim.')
        for role in ('client', 'node1', 'node2', 'node3'):
            path = self.c / f'provenance/f0-{role}/runtime.sha256'
            lines = [line.split() for line in path.read_text().splitlines() if line.strip()]
            need(lines and all(len(line) == 2 for line in lines), 'missing/malformed runtime receipt')
            self.provenance['runtime_receipts'][role] = dict(path=str(path), sha256=sha(path), identities={p: h for h, p in lines})
        baseline = self.provenance['runtime_receipts']['client']['identities']
        required = {'/usr/sbin/fdbserver', '/usr/bin/fdbcli', '/usr/lib/libfdb_c.so'}
        need(required.issubset(baseline) and all(all(r['identities'].get(p) == baseline[p] for p in required)
             for r in self.provenance['runtime_receipts'].values()), 'incomplete/inconsistent F0 runtime identities')
        self.out.mkdir(parents=True, exist_ok=False)

    def controller(self, phase, expected):
        folder = self.c / 'phase-controller' / phase
        complete, inputs, manifest = (read(folder / name) for name in ('complete.json', 'inputs.json', 'manifest.json'))
        need(complete.get('phase') == phase and complete.get('label') == phase and complete.get('diagnostic') is False,
             'diagnostic/wrong controller phase refused')
        need(complete.get('runner_quiescent') is True and complete.get('errors') == [] and complete.get('reconcile_rc') in (0, 3),
             'controller cannot establish completed quiescent collection/R4')
        need(inputs.get('phase') == phase and inputs.get('diagnostic') is False and inputs.get('expected_cases') == expected,
             'controller expected population differs from protocol')
        need(inputs['manifest_sha256'] == sha(folder / 'manifest.json'), 'acknowledged controller manifest changed')
        pins = manifest.get('pinned_files', {})
        need(pins, 'empty execution pin population')
        need(pins.get(inputs['protocol']) == sha(self.protocol_path), 'controller sealed a different protocol')
        if 'protocol_sha256' in manifest:
            need(manifest['protocol_sha256'] == sha(self.protocol_path), 'controller protocol hash disagrees with pins')
        for tool in self.tool_paths:
            remote = '/srv/fdb-bench/' + str(tool.relative_to(self.c))
            if remote in pins:
                need(pins[remote] == sha(tool), 'analysis/runtime helper differs from sealed code: ' + str(tool))
        live = read(folder / 'client-pins-before-r4.json')
        need(live.get('errors') == [] and live.get('actual') == pins, 'missing/mismatched final remote pin verification')
        return manifest, dict(controller_manifest_sha256=sha(folder / 'manifest.json'),
                              controller_complete_sha256=sha(folder / 'complete.json'), sealed_pins=pins)

    def historical(self, phase, key, current, case=None):
        note = dict(interpretation=DESCRIPTIVE, status='unavailable')
        try:
            old_source_path = self.prior / 'provenance/source-receipt.json'
            old_source = read(old_source_path)
            need(old_source['production_revisions']['final'] == self.protocol['production_revisions']['before_allocation_work_after_read_parity'],
                 'historical final does not name the current previous production revision')
            note['historical_source_receipt_sha256'] = sha(old_source_path)
            rec_path = self.prior / f'reconcile/{phase}/report.json'
            rec = read(rec_path)
            need(rec.get('reconciliation_complete') is True and rec.get('phase') == phase, 'historical R4 incomplete')
            matches = [c for c in rec['cases'] if c['case'] == key]
            need(len(matches) == 1 and matches[0]['verdict'] == 'clean', 'historical case not clean')
            if phase.startswith('M1'):
                path = self.prior / f'analysis/{phase}/summary.json'
                old = read(path)['workloads'][key]
                need(old['status'] == 'clean', 'historical analysis omitted case')
                workers = next(p['workers'] for p in self.protocol['phases'] if p['id'] == phase)
                need(old['per_run'] and all(r['workers'] == workers and r['workload'] == key for r in old['per_run']),
                     'historical worker/workload geometry differs')
                pairs = [dict(metric=c['metric'], current_numerator=c['numerator'], current_denominator=c['denominator'],
                              current=c, historical=old['comparisons'][c['metric']], historical_numerator='final (267a4143)', historical_denominator='cgo')
                         for c in current if c['denominator'] == 'cgo' and c['metric'] in old['comparisons']]
            else:
                path = self.prior / f'analysis/{phase}/report.json'
                old = read(path)
                need(old['cases'][key]['status'] == 'ok', 'historical analysis omitted case')
                sealed = read(self.prior / f'plans/testingb/{phase}.plan.json')
                old_case = next(c for c in sealed['cases'] if c['key'] == key)
                fields = ('stem', 'payload', 'cell', 'dir', 'store', 'layout', 'record_format')
                need(all(case.get(k) == old_case.get(k) for k in fields), 'historical/new case geometry differs')
                lookup = {(c['metric'], c['numerator'], c['denominator']): c for c in old['comparisons'] if c['case'] == key and c.get('status') == 'ok'}
                pairs = [dict(metric=c['metric'], current=c, historical=lookup[c['metric'], c['numerator'], c['denominator']])
                         for c in current if (c['metric'], c['numerator'], c['denominator']) in lookup]
                note['historical_placement'] = {k: old_case.get(k) for k in ('placement_label', 'placement_fraction')}
                note['current_placement'] = {k: case.get(k) for k in ('placement_label', 'placement_fraction')}
            need(pairs, 'no matching historical contrasts')
            note.update(status='descriptive-only', records=pairs, historical_analysis_sha256=sha(path), historical_r4_sha256=sha(rec_path))
        except (OSError, KeyError, ValueError, StopIteration) as exc:
            note['reason'] = str(exc)
        return note

    def client_case(self, phase, spec, key, raw, rec, dest, manifest):
        adverse = [p.name for p in raw.glob('*ADVERSE*.json')] + [p.name for p in raw.glob('*ABORTED*.json')]
        need(not adverse and not (raw / 'UNEXECUTED.json').exists(), 'runner adverse/unexecuted case: ' + str(adverse))
        plan, schedule, complete = (read(raw / name) for name in ('plan.json', 'schedule.json', 'complete.json'))
        need(plan['blocks'] == BLOCKS and {a['name'] for a in plan['arms']} == {'before', 'final', 'cgo'}, 'wrong client arms/blocks')
        need(len(plan['workloads']) == 1 and plan['workloads'][0]['name'] == key and plan['workloads'][0]['workers'] == spec['workers'], 'wrong workload/worker level')
        revisions = {a['name']: a['source_revision'] for a in plan['arms']}
        production = self.protocol['production_revisions']
        need(revisions['before'] == production['before_allocation_work_after_read_parity'] and revisions['final'] == revisions['cgo'] == production['final'], 'client production identity disagreement')
        observed = [(r['block'], r['arm'], r['workload']['name']) for r in schedule]
        expected = {(b, a, key) for b in range(BLOCKS) for a in ('before', 'final', 'cgo')}
        need(len(observed) == 18 and set(observed) == expected and complete['scheduled'] == complete['completed'] == 18, 'incomplete whole client case')
        expected_binaries = {'before': 'old/client-go', 'final': 'master/client-go', 'cgo': 'master/client-cgo'}
        for arm in plan['arms']:
            need(plan['expected_sha256'][arm['binary']] == self.binary_hashes[expected_binaries[arm['name']]], 'client plan binary differs from build receipt')
        native = self.provenance['runtime_receipts']['client']['identities']['/usr/lib/libfdb_c.so']
        need(plan['native_library_sha256'] == native, 'client native library differs from runtime receipt')
        exact_runs(rec, [r['run_id'] for r in schedule])
        entry = [c for c in manifest['campaigns'] if c['phase'] == phase and c['workload'] == key]
        need(len(entry) == 1 and entry[0]['plan_sha256'] == sha(raw / 'plan.json'), 'client plan differs from sealed manifest')
        adapter = read(raw / 'adapter.json')
        need(adapter['manifest_sha256'] == sha(self.c / f'phase-controller/{phase}/manifest.json') and adapter['protocol_sha256'] == sha(self.protocol_path), 'raw adapter identity mismatch')
        no_symlinks(raw)
        reports = {}
        for denominator in ('cgo', 'before'):
            target = dest / ('denom-' + denominator)
            shutil.copytree(raw, target)
            command = [sys.executable, '-B', str(self.scripts / 'analyze.py'), str(target), '--denominator', denominator,
                       '--seed', str(SEED), '--resamples', str(RESAMPLES)]
            completed = self.run(command, capture_output=True, text=True, timeout=600,
                                 env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'))
            (dest / ('denom-' + denominator + '.stdout')).write_text(completed.stdout)
            (dest / ('denom-' + denominator + '.stderr')).write_text(completed.stderr)
            need(completed.returncode == 0, 'frozen client analyzer refused ' + key)
            report = read(target / 'analysis.json')
            need(report['valid_zero_error_campaign'] is True and report['issues'] == [] and report['bootstrap_seed'] == SEED and report['bootstrap_resamples'] == RESAMPLES, 'client analyzer did not produce eligible result')
            paired_rows(report['per_run'], key)
            reports[denominator] = report
        need(reports['before']['per_run'] == reports['cgo']['per_run'], 'denominator analyses disagree on raw rows')
        comparisons = [c for report in reports.values() for c in report['comparisons'] if (c['numerator'], c['denominator']) in CONTRASTS]
        unavailable = [c for report in reports.values() for c in report['unavailable_comparisons'] if (c['numerator'], c['denominator']) in CONTRASTS]
        signatures = [(c['numerator'], c['denominator'], c['metric']) for c in comparisons + unavailable]
        need(len(signatures) == len(CLIENT_METRICS) * len(CONTRASTS) and
             set(signatures) == {(n, d, m) for n, d in CONTRASTS for m in CLIENT_METRICS}, 'missing/duplicate client comparison metrics')
        need(all(c['blocks'] == BLOCKS for c in comparisons), 'client comparison used a reduced block population')
        comparisons += allocation_comparisons(self.ca, reports['cgo']['per_run'], key)
        comparison_csv(dest / 'paired-all.csv', comparisons)
        return dict(status='clean', comparisons=comparisons, per_run=reports['cgo']['per_run'],
                    unavailable_comparisons=unavailable,
                    historical=self.historical(phase, key, comparisons))

    def testingb(self, phase, spec, raw, recs, dest, plan):
        need(spec['blocks'] == BLOCKS and spec['repeats_per_arm_process'] == 3 and len(plan['orders']) == BLOCKS,
             'wrong R2 blocks/repeats')
        need(all(set(order) == {'final-ordinary', 'final-remote'} and len(order) == 2 for order in plan['orders']), 'wrong R2 arms')
        samples = self.ba.load_samples(raw / 'metrics.csv')
        omitted = read(raw / 'omitted.json')
        cases = {c['key']: c for c in plan['cases']}
        need(len(plan['jobs']) == 12 * len(cases) and len({j['id'] for j in plan['jobs']}) == len(plan['jobs']) and
             {j['case'] for j in plan['jobs']} == set(cases), 'invalid sealed R2 job population')
        need(set(omitted).issubset(cases), 'unknown runner omission')
        for key, rec in recs.items():
            if rec['verdict'] != 'clean':
                omitted[key] = dict(reason='R4 ' + rec['verdict'], reasons=rec.get('contaminated_reasons', []) + rec.get('unverifiable_reasons', []))
        for key in cases:
            if key in omitted:
                continue
            jobs = [j for j in plan['jobs'] if j['case'] == key]
            identities = [(j['block'], j['arm']) for j in jobs]
            need(len(identities) == 12 and set(identities) == {(b, a) for b in range(BLOCKS) for a in spec['arms']}, 'incomplete R2 whole case')
            exact_runs(recs[key], [j['id'] for j in jobs])
            for job in jobs:
                folder = raw / job['id']
                need(read(folder / 'result.json')['status'] == 'ok', 'non-successful R2 raw process')
                view = dict(job, case=self.mr.audit_case(cases[key]))
                parsed, audits = self.br.parse_output((folder / 'stdout.txt').read_text() + '\n' + (folder / 'stderr.txt').read_text(),
                                                     view, repeats=3, gomaxprocs=plan['env']['GOMAXPROCS'])
                rows = sorted((r for r in samples if r['case'] == key and r['block'] == job['block'] and r['arm'] == job['arm']), key=lambda r: r['repeat'])
                need(len(rows) == 3 and all(r['N'] == p['N'] and all(r[m] == p['metrics'].get(m) for m in self.br.METRICS) for r, p in zip(rows, parsed)), 'R2 CSV differs from raw benchmark population')
                need(read(folder / 'audits.json') == audits and read(folder / 'parsed.json') == parsed, 'R2 saved parse/audit differs from raw log')
                for row, parsed_row in zip(rows, parsed):
                    row['audit_prefix_bytes'] = parsed_row['measured_prefix_bytes']
        if not omitted:
            complete = read(raw / 'complete.json')
            need(complete['scheduled'] == complete['completed'] == len(plan['jobs']) and complete['samples'] == len(samples) and
                 complete['pins_unchanged'] is True and complete['plan_sha256'] == sha(raw / 'plan.json'), 'R2 complete receipt differs from raw population')
        adapted = dict(phase=dict(id=phase, question='Multi-node VPC remote/ordinary; whole-case R4 gated', arms=spec['arms'], blocks=BLOCKS),
                       cases=plan['cases'], orders=plan['orders'], production_revisions=self.protocol['production_revisions'],
                       harness_commit=self.protocol['frozen_harness_revision'], bootstrap_resamples=RESAMPLES, analysis_seed=SEED,
                       interpretation='testing.B ns/op is a mean, not p99; median of three repeats per process, six paired blocks.')
        report = self.ba.summarize(adapted, samples, omitted)
        for key, value in report['cases'].items():
            value.update({k: cases[key].get(k) for k in ('placement_label', 'placement_fraction')})
            if value['status'] == 'ok':
                comps = [c for c in report['comparisons'] if c['case'] == key]
                value['historical'] = self.historical(phase, key, comps, cases[key])
        for comparison in report['comparisons']:
            comparison.update({k: cases[comparison['case']].get(k) for k in ('placement_label', 'placement_fraction')})
        report.update(r4_reconciliation_sha256=sha(self.c / f'reconcile/{phase}/report.json'), metrics_sha256=sha(raw / 'metrics.csv'))
        dump(dest / 'report.json', report)
        comparison_csv(dest / 'paired.csv', report['comparisons'])
        return report

    def phase(self, phase, omitted_reason=None):
        specs = [p for p in self.protocol['phases'] if p['id'] == phase]
        need(len(specs) == 1 and phase in ('M1w1', 'M1w8', 'M1w32', 'M1w64', 'M2rl', 'M2skip'), 'unknown/diagnostic phase')
        spec, raw = specs[0], self.c / 'results-raw' / phase
        dest = self.out / phase
        dest.mkdir()
        if not raw.exists() or (raw / 'UNEXECUTED.json').exists():
            result = dict(status='N/A', reason=omitted_reason or 'not executed or not collected; no diagnostic/capacity substitution')
            dump(dest / 'summary.json', result)
            return result
        need(omitted_reason is None, 'cannot hide existing raw phase under a budget omission')
        no_symlinks(raw)
        input_hashes = tree_hashes(raw)
        plan = read(raw / 'plan.json') if spec['kind'] == 'testingb' else None
        expected = spec['workloads'] if plan is None else [c['key'] for c in self.sm.testingb_cells(spec)]
        if plan is not None:
            need(plan['phase'] == phase and [c['key'] for c in plan['cases']] == expected, 'R2 plan case population differs from protocol')
        manifest, provenance = self.controller(phase, expected)
        rec_path = self.c / f'reconcile/{phase}/report.json'
        rec = read(rec_path)
        cases = r4_cases(rec, phase, expected)
        need(read(rec_path.with_name('input.json'))['expected_cases'] == expected, 'R4 input population differs from protocol')
        need(all(rec['tool_sha256'].get(name) == sha(self.mn / name) for name in ('reconcile_telemetry.py', 'node_sampler.py')),
             'R4 tool identity differs')
        provenance.update(r4_reconciliation_sha256=sha(rec_path), r4_input_sha256=sha(rec_path.with_name('input.json')))
        if plan is not None:
            entries = [p for p in manifest['phases'] if p['phase'] == phase]
            need(len(entries) == 1 and entries[0]['plan_sha256'] == sha(raw / 'plan.json'), 'R2 plan differs from controller manifest')
            pins = read(raw / 'pins.before.json')
            after = raw / 'pins.after.json'
            final_pins = read(after if after.exists() else raw / 'pins.finally.json')
            need(pins == plan['expected_sha256'] and all(final_pins.get(k) == v for k, v in pins.items()), 'R2 phase pins changed')
            binaries = {job['command'][3] for job in plan['jobs']}
            need(len(binaries) == 1 and pins[next(iter(binaries))] == self.binary_hashes['master/' + spec['binary_kind']],
                 'R2 benchmark binary differs from build receipt')
            result = self.testingb(phase, spec, raw, cases, dest, plan)
        else:
            workloads = {}
            for key in expected:
                case = cases[key]
                if case['verdict'] != 'clean':
                    workloads[key] = dict(status='N/A', r4_verdict=case['verdict'], reasons=case.get('contaminated_reasons', []) + case.get('unverifiable_reasons', []))
                    continue
                case_dest = dest / key
                case_dest.mkdir()
                workloads[key] = self.client_case(phase, spec, key, raw / key, case, case_dest, manifest)
            result = dict(phase=phase, workloads=workloads, r4_reconciliation_sha256=sha(rec_path))
            dump(dest / 'summary.json', result)
        need(tree_hashes(raw) == input_hashes, 'raw input tree changed during analysis')
        provenance['raw_artifacts_sha256'] = input_hashes
        dump(dest / 'analysis-provenance.json', provenance)
        return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--campaign', type=Path, default=DEFAULT)
    parser.add_argument('--protocol', type=Path, help='default campaign/deploy/protocol.json')
    parser.add_argument('--prior', type=Path, default=PRIOR)
    parser.add_argument('--output', type=Path, required=True, help='fresh directory below campaign/analysis/')
    parser.add_argument('--phase', action='append', required=True)
    parser.add_argument('--omitted-phase', action='append', default=[], metavar='PHASE=REASON')
    args = parser.parse_args(argv)
    os.umask(0o077)
    adapter = None
    try:
        need(len(set(args.phase)) == len(args.phase), 'duplicate requested phases')
        omitted = dict(value.split('=', 1) for value in args.omitted_phase)
        need(set(omitted).issubset(args.phase) and all(omitted.values()), 'invalid omitted-phase reason')
        adapter = Adapter(args.campaign, args.protocol or args.campaign / 'deploy/protocol.json', args.output, args.prior)
        summary = dict(status='complete', phases={}, provenance=adapter.provenance, historical_interpretation=DESCRIPTIVE)
        for phase in args.phase:
            summary['phases'][phase] = adapter.phase(phase, omitted.get(phase))
        eligible = sum(sum(v.get('status') == 'clean' for v in result.get('workloads', {}).values()) +
                       sum(v.get('status') == 'ok' for v in result.get('cases', {}).values()) for result in summary['phases'].values())
        summary['eligible_cases'] = eligible
        summary['performance_results_available'] = eligible > 0
        if eligible == 0:
            summary['status'] = 'complete-no-eligible-cases'
        need({str(p): sha(p) for p in adapter.tool_paths} == adapter.provenance['tool_sha256'], 'analysis helper code changed during analysis')
        need(sha(adapter.protocol_path) == adapter.provenance['protocol_sha256'] and
             sha(adapter.source_path) == adapter.provenance['source_receipt_sha256'] and
             sha(adapter.build_path) == adapter.provenance['build_receipt_sha256'], 'analysis source/protocol provenance changed')
        dump(adapter.out / 'analysis-provenance.json', adapter.provenance)
        dump(adapter.out / 'summary.json', summary)
        print(str(adapter.out / 'summary.json'))
        return 0
    except Exception as exc:
        if adapter is not None:
            dump(adapter.out / 'REFUSED.json', dict(error=str(exc), no_clean_result=True))
        print('REFUSED: ' + str(exc), file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
