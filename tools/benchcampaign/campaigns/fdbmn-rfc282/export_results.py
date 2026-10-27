#!/usr/bin/env python3
"""Offline full-matrix export. No benchmark, SSH or cloud operation is launched."""
import argparse
from decimal import Decimal, InvalidOperation
import hashlib
import json
import math
from pathlib import Path
import sys

from helpers import legacy_io, legacy_m1
import m2_export
import server_cpu

WORKLOADS = ('grv', 'get100', 'get1000', 'range100', 'set100', 'batchset50', 'batchset50x1000')
QUOTAS = {1: (3000, 200), 8: (12000, 800), 32: (24000, 1600), 64: (32000, 1600)}
ARMS = ('before', 'final', 'cgo')
METRICS = ('mean_ns', 'p50_ns', 'p99_ns', 'shutdown_process_cpu_ns_per_op',
           'go_alloc_bytes_per_op', 'go_mallocs_per_op')
SEED, RESAMPLES = 492817, 20000
need = server_cpu.need


def protocol_cases(protocol):
    """This campaign is exactly the prior matrix, not whatever data survived."""
    phases = protocol['phases']
    specs = {p['id']: p for p in phases}
    need(len(specs) == len(phases) == 6 and set(specs) ==
         {'M1w1', 'M1w8', 'M1w32', 'M1w64', 'M2rl', 'M2skip'}, 'wrong full campaign phase population')
    expected = {}
    for workers, (iterations, warmup) in QUOTAS.items():
        phase = f'M1w{workers}'
        spec = specs[phase]
        need((spec['kind'], spec['workers'], spec['iterations'], spec['warmup'], spec['blocks'], spec['arms']) ==
             ('client', workers, iterations, warmup, 6, ['before-Go', 'final-Go', 'CGo-7.3.77'])
             and tuple(spec['workloads']) == WORKLOADS, 'changed frozen M1 geometry')
        expected[phase] = list(WORKLOADS)
    for phase in ('M2rl', 'M2skip'):
        spec = specs[phase]
        need(spec['kind'] == 'testingb' and spec['blocks'] == 6 and spec['repeats_per_arm_process'] == 3
             and spec['benchtime'] == '3s' and spec['arms'] == ['final-ordinary', 'final-remote']
             and spec['dirs'] == ['forward', 'reverse'], 'changed frozen M2 geometry')
        if phase == 'M2rl':
            need(spec['payloads'] == ['small', 'split'] and spec['cells'] == ['limit1', 'rows10', 'rows1000'],
                 'changed M2rl cells')
            expected[phase] = [f'{p}-{c}-{d}' for p in spec['payloads'] for c in spec['cells'] for d in spec['dirs']]
        else:
            need(spec['cells'] == ['rows10', 'rows1000'], 'changed M2skip cells')
            expected[phase] = [f'skip3-{c}-{d}' for c in spec['cells'] for d in spec['dirs']]
    need(sum(map(len, expected.values())) == 44, 'empty/incomplete campaign')
    policy = protocol['sample_policy']
    need((policy['analysis_seed'], policy['bootstrap_resamples']) == (SEED, RESAMPLES), 'changed analysis policy')
    return specs, expected


def public_r4_reasons(reasons):
    """Keep diagnostic categories public; exact diagnostics stay in pinned R4."""
    need(isinstance(reasons, list) and all(isinstance(reason, str) for reason in reasons),
         'invalid R4 exclusion reasons')
    categories = (('role or process-class change', 'FDB role or process-class changed'),
                  ('placement', 'fixture placement qualification failed'),
                  ('steal', 'CPU steal contamination'),
                  ('clock', 'clock qualification failed'),
                  ('MainPID', 'FDB process identity changed'),
                  ('missing', 'required evidence missing'),
                  ('no case receipts', 'case receipts missing'),
                  ('samples', 'telemetry sample qualification failed'))
    return [next((label for marker, label in categories if marker in reason),
                 'R4 exclusion; diagnostic retained in pinned private report')
            + ' [sha256:' + hashlib.sha256(reason.encode()).hexdigest() + ']'
            for reason in reasons]


def population(inputs, analysis, expected):
    coverage, reports, analyses = {}, {}, {}
    for phase, cases in expected.items():
        report = inputs.load(f'reconcile/{phase}/report.json')
        need(report.get('reconciliation_complete') is True and report.get('phase') == phase, 'incomplete R4')
        r4 = {c['case']: c for c in report['cases']}
        need(len(r4) == len(report['cases']) and set(r4) == set(cases), 'R4 case population differs')
        current = inputs.load(f'{analysis}/{phase}/' + ('summary.json' if phase.startswith('M1') else 'report.json'))
        need(current.get('r4_reconciliation_sha256') == inputs.digest(f'reconcile/{phase}/report.json'),
             'canonical analysis refers to a different R4 report')
        current_cases = current['workloads'] if phase.startswith('M1') else current['cases']
        need(set(current_cases) == set(cases), 'analysis case population differs')
        coverage[phase] = {}
        for case in cases:
            verdict = r4[case]['verdict']
            need(verdict in ('clean', 'contaminated', 'unverifiable'), 'invalid R4 verdict')
            clean = current_cases[case]['status'] == ('clean' if phase.startswith('M1') else 'ok')
            need(clean == (verdict == 'clean'), 'R4/analysis eligibility differs')
            need(not clean or not (r4[case]['contaminated_reasons'] + r4[case]['unverifiable_reasons']),
                 'clean case has adverse reasons')
            coverage[phase][case] = dict(verdict=verdict,
                reasons=public_r4_reasons(r4[case]['contaminated_reasons'] + r4[case]['unverifiable_reasons']))
        reports[phase], analyses[phase] = report, current
    counts = {v: sum(c['verdict'] == v for p in coverage.values() for c in p.values())
              for v in ('clean', 'contaminated', 'unverifiable')}
    need(sum(counts.values()) == 44, 'coverage partition differs')
    return dict(planned=44, **counts, phases=coverage), reports, analyses


def collect_m1(inputs, specs, expected, analyses, reports, bootstrap):
    output, comparisons = [], []
    for phase in (p for p in expected if p.startswith('M1')):
        spec = specs[phase]
        recs = {r['case']: r for r in reports[phase]['cases']}
        for case in expected[phase]:
            retained, rec = analyses[phase]['workloads'][case], recs[case]
            if rec['verdict'] != 'clean':
                continue
            source = retained['per_run']
            schedule = inputs.load(f'results-raw/{phase}/{case}/schedule.json')
            ids = [r['run_id'] for r in source]
            need(len(ids) == len(set(ids)) == len(schedule) == 18
                 and {r['run_id'] for r in schedule} == set(ids)
                 and len(rec['runs']) == 18 and {r['run_id'] for r in rec['runs']} == set(ids), 'incomplete M1 case')
            jobs = {r['run_id']: r for r in schedule}
            processes = []
            for r in source:
                job = jobs[r['run_id']]
                need((job['block'], job['position'], job['arm'], job['workload']['name'], job['workload']['workers']) ==
                     (r['block'], r['position'], r['arm'], case, spec['workers'])
                     and r['workload'] == case and r['workers'] == spec['workers'], 'M1 process identity differs')
                raw = inputs.load(f'results-raw/{phase}/{case}/{r["run_id"]}/summary.json')
                need(raw['requested'] == job['workload']['iterations'] == spec['iterations']
                     and raw['warmup_completed'] == job['workload']['warmup'] == spec['warmup'], 'wrong M1 quota')
                samples = inputs.text(f'results-raw/{phase}/{case}/{r["run_id"]}/samples.csv')
                row = legacy_m1.measured_process(raw, r, samples)
                row.update(phase=phase, case=case)
                for metric in METRICS:
                    value = r[metric]
                    need(type(value) in (int, float) and math.isfinite(value) and value >= 0, 'invalid M1 metric')
                    row[metric] = value
                processes.append(row)
            paired = legacy_m1.paired_comparisons(processes, bootstrap)
            comparisons.extend(dict(phase=phase, case=case, **r) for r in paired)
            # Latency/lifecycle/allocation comparisons are already estimated by
            # the pinned frozen adapter, never quotients of absolute medians.
            retained_comparisons = retained['comparisons'] + retained.get('unavailable_comparisons', [])
            wanted = {(n, d, m) for n, d in legacy_m1.CONTRASTS for m in METRICS}
            selected = [r for r in retained_comparisons if (r['numerator'], r['denominator'], r['metric']) in wanted]
            need(len(selected) == len(wanted) and
                 {(r['numerator'], r['denominator'], r['metric']) for r in selected} == wanted,
                 'missing/duplicate retained metric comparisons')
            for row in selected:
                clean = {k: row[k] for k in ('metric', 'numerator', 'denominator', 'ratio', 'ci95_low', 'ci95_high', 'blocks', 'status') if k in row}
                need(clean.get('blocks', 6) == 6, 'reduced comparison blocks')
                comparisons.append(dict(phase=phase, case=case, **clean))
            output.extend(processes)
    return output, comparisons


def measured_window(raw):
    need(raw.get('measured_fences_format') == 'fdb-bench-measured-fences/v1', 'missing reviewed measured fences')
    start, end = raw['measured_started_unix_ns'], raw['measured_ended_unix_ns']
    server_cpu.integer(raw['measured_wall_ns'], 'monotonic measured wall', 1)
    server_cpu.integer(start, 'measured start', 1)
    server_cpu.integer(end, 'measured end', 1)
    need(start == server_cpu.utc_ns(raw['measured_started_utc']) and end == server_cpu.utc_ns(raw['measured_ended_utc']),
         'UTC/Unix measured fences differ')
    # Wall clock steps are not repaired by replacing the authoritative monotonic wall.
    need(end > start and abs(end - start - raw['measured_wall_ns']) <= 1000, 'wall/monotonic measured fences differ')
    return start, end


def cpu_failure(error):
    if isinstance(error, server_cpu.CPUUnqualified):
        return str(error)
    # File/JSON/parser exceptions can include private paths or raw values.
    return 'unavailable or invalid pinned CPU evidence (' + type(error).__name__ + ')'


def cpu_rows(inputs, phase, report, layout, processes, parser):
    units = server_cpu.unit_population(layout)
    nodes = {}
    try:
        for node in server_cpu.NODES:
            root = f'telemetry-cpu/{phase}/{node}'
            start, stop = (server_cpu.mapping(inputs.load(f'{root}/receipt-{k}.json'), 'sidecar receipt')
                           for k in ('start', 'stop'))
            for kind, sidecar in (('start', start), ('stop', stop)):
                relative = f'telemetry/{phase}/{node}/receipt-{kind}.json'
                base = server_cpu.mapping(inputs.load(relative), 'R4 sampler receipt')
                server_cpu.mapping(base.get('units'), 'R4 sampler units')
                server_cpu.mapping(sidecar.get('units'), 'sidecar units')
                need(report['inputs_sha256']['/srv/fdb-bench/' + relative] == inputs.digest(relative),
                     'R4/base receipt hash differs')
                need(base['units']['starttimes'] == sidecar['units']['starttimes']
                     and base['boot_id'] == sidecar['boot_id'], 'R4/sidecar process identity differs')
            text = inputs.text(f'{root}/samples.jsonl')
            need(hashlib.sha256(text.encode()).hexdigest() == stop['samples_sha256'], 'sidecar stop hash differs')
            samples = [json.loads(line) for line in text.splitlines()]
            data = server_cpu.validate_node(start, stop, samples, node, phase, units[node], max_gap_ns=20_000_000)
            bounds = []
            for receipt in (start, stop):
                server_cpu.mapping(receipt.get('chrony'), 'sidecar chrony receipt')
                need(receipt['chrony']['rc'] == 0, 'sidecar chrony failed')
                fields, errors = parser.parse_chrony_tracking(receipt['chrony']['raw'])
                need(not errors, 'sidecar chrony invalid')
                bounds.append(parser.chrony_error_bound(fields))
            client_bound = Decimal(str(report['clock']['client']['bound_s']))
            need(client_bound.is_finite() and client_bound >= 0, 'undefined client clock bound')
            data['clock_bound_s'] = client_bound + max(bounds)
            need(set(report['nodes'][node]['units']) == units[node], 'R4/sidecar unit population differs')
            nodes[node] = data
    except (KeyError, ValueError, TypeError, AttributeError, OSError, InvalidOperation) as error:
        return [dict(phase=phase, case=c['case'], status='CPU-unqualified', reason=cpu_failure(error))
                for c in report['cases'] if c['verdict'] == 'clean']
    output = []
    by_run = {r.get('run_id', r.get('source_run_id')): r for r in processes if r['phase'] == phase}
    for case in report['cases']:
        if case['verdict'] != 'clean':
            continue
        case_rows = []
        try:
            for run in case['runs']:
                row = by_run[run['run_id']]
                folder = f'results-raw/{phase}/' + (case['case'] + '/' if phase.startswith('M1') else '') + run['run_id']
                process_receipt = server_cpu.mapping(inputs.load(folder + '/result.json'), 'raw process receipt')
                need((process_receipt['run_id'], process_receipt['started_utc'], process_receipt['ended_utc']) ==
                     (run['run_id'], run['start_utc'], run['end_utc']), 'R4/raw lifecycle window differs')
                if phase.startswith('M1'):
                    raw = server_cpu.mapping(inputs.load(folder + '/summary.json'), 'raw measured summary')
                    window = measured_window(raw)
                else:
                    window = None
                cpu = server_cpu.estimate_run(run, nodes, window)
                if window is not None:
                    ops = server_cpu.integer(row['measured_ops'], 'completed measured operations', 1)
                    cpu.update(lower_cpu_ns_per_op=cpu['lower_cpu_seconds'] * 1e9 / ops,
                               upper_cpu_ns_per_op=cpu['upper_cpu_seconds'] * 1e9 / ops)
                case_rows.append(dict(phase=phase, case=case['case'], arm=row['arm'], block=row['block'], status='qualified', **cpu))
        except (KeyError, ValueError, TypeError, AttributeError, OSError, InvalidOperation) as error:
            output.append(dict(phase=phase, case=case['case'], status='CPU-unqualified', reason=cpu_failure(error)))
            continue  # whole case: never publish just the surviving arms
        output.extend(case_rows)
    return output


def export(inputs, analysis, bootstrap, paired_gm, parser):
    inputs.verify_all()
    protocol = inputs.load('deploy/protocol.json')
    specs, expected = protocol_cases(protocol)
    coverage, reports, analyses = population(inputs, analysis, expected)
    source = inputs.load('provenance/source-receipt.json')
    need(source['production_revisions'] == protocol['production_revisions'], 'source/protocol revisions differ')
    canonical = inputs.load(f'{analysis}/summary.json')
    provenance = canonical['provenance']
    need(canonical['status'] in ('complete', 'complete-no-eligible-cases') and set(canonical['phases']) == set(expected)
         and canonical['eligible_cases'] == coverage['clean']
         and all(canonical['phases'][p] == analyses[p] for p in expected)
         and provenance['protocol_sha256'] == inputs.digest('deploy/protocol.json')
         and provenance['source_receipt_sha256'] == inputs.digest('provenance/source-receipt.json'),
         'canonical adapter/source/protocol provenance differs')
    runtime_hashes = {role: {Path(name).name: receipt['identities'][name] for name in
                            ('/usr/sbin/fdbserver', '/usr/bin/fdbcli', '/usr/lib/libfdb_c.so')}
                      for role, receipt in provenance['runtime_receipts'].items()}
    need(set(runtime_hashes) == {'client', *server_cpu.NODES}, 'missing canonical runtime population')
    extension = protocol['measurement_extensions']
    need(extension['throughput_preregistered'] is True and extension['server_cpu_source'] == 'exclusive-cgroup-v2-usage-usec'
         and extension['process_cpu_interval_ns'] == 10_000_000
         and extension['process_cpu_min_inner_intervals'] == 10 and extension['process_cpu_max_time_envelope_ratio'] == 1.2
         and extension['process_cpu_clock_reserve_ns'] == server_cpu.CLOCK_RESERVE_NS
         and extension['process_cpu_phase_correlation_range_ns'] == server_cpu.MAX_PHASE_CLOCK_RANGE_NS,
         'unsealed measurement extension policy')
    m1, comparisons = collect_m1(inputs, specs, expected, analyses, reports, bootstrap)
    m2, m2_method = m2_export.collect(inputs, paired_gm, {p: c for p, c in expected.items() if p.startswith('M2')}, analysis)
    layout = inputs.load('cluster-ssd/layout.json')
    cpu = []
    for phase in expected:
        cpu += cpu_rows(inputs, phase, reports[phase], layout, m1 if phase.startswith('M1') else
                        [r for r in m2 if r['record_type'] == 'process'], parser)
    # Run IDs can contain campaign nonces. They are private join keys only.
    for row in m1 + m2:
        row.pop('run_id', None)
        row.pop('source_run_id', None)
    # Retained M2 means/allocations are separately scoped from throughput/CPU.
    m2_metrics, m2_absolutes = [], []
    for phase in ('M2rl', 'M2skip'):
        for case, result in analyses[phase]['cases'].items():
            if coverage['phases'][phase][case]['verdict'] != 'clean':
                continue
            need(set(result['absolutes']) == set(m2_export.ARMS), 'missing M2 absolute arm')
            for arm, metrics in result['absolutes'].items():
                values = {m: metrics[m]['median_of_process_repeat_medians'] for m in
                          ('ns/op', 'B/op', 'allocs/op', 'rows/op', 'prefix-B')}
                need(all(type(v) in (int, float) and math.isfinite(v) and v >= 0 for v in values.values()),
                     'invalid retained M2 absolute')
                m2_absolutes.append(dict(phase=phase, case=case, arm=arm, blocks=6, repeats_per_process=3, metrics=values))
        for r in analyses[phase]['comparisons']:
            if coverage['phases'][phase][r['case']]['verdict'] == 'clean':
                m2_metrics.append(dict(phase=phase, **{k: r[k] for k in
                    ('case', 'metric', 'numerator', 'denominator', 'gm', 'nominal_95_interval', 'blocks', 'status') if k in r}))
    inputs.verify_all()
    return dict(format='fdbmn-rfc282-public-results/v1', performance_results_available=coverage['clean'] > 0,
                coverage=coverage, M1=m1, M1_comparisons=comparisons,
                M2=m2, M2_absolutes=m2_absolutes, M2_comparisons=m2_metrics, server_process_cpu=cpu,
                method=dict(production_revisions=source['production_revisions'], harness_commit=source['harness_commit'],
                    compiler_sha256=source['compiler_sha256'], runtime_binary_sha256=runtime_hashes,
                    runtime={k: protocol['runtime'][k] for k in ('go', 'bazel', 'bazelisk', 'foundationdb', 'api',
                             'GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG', 'TZ', 'LC_ALL')},
                    harness_amendment={k: source['harness_compatibility'][k] for k in ('upstream_commit', 'archive_sha256', 'patch_sha256')},
                    absolute_estimand='median of run-level statistics; M2 median of process repeat means; never pooled percentiles',
                    preregistered=True, bootstrap_seed=SEED, resamples=RESAMPLES, paired_blocks=6,
                    M2=m2_method, server_cpu='exclusive cgroup-v2 usage_usec bounds only; never add host/proc diagnostics; measured M1 and lifecycle M2',
                    native_heap='not measured by Go allocation counters', inference='nominal exploratory intervals; no equivalence/capacity claim'))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__)
    for name in ('campaign', 'inputs-manifest', 'analysis', 'output-dir', 'redactions'):
        ap.add_argument('--' + name, required=True)
    args = ap.parse_args(argv)
    legacy_io.relative_path(args.analysis)
    inputs = legacy_io.VerifiedInputs(args.campaign, args.inputs_manifest)
    client = legacy_m1.load_module(inputs, legacy_m1.CLIENT_ANALYZER, legacy_m1.CLIENT_RUNNER)
    testingb = legacy_m1.load_module(inputs, legacy_m1.M2_ANALYZER, legacy_m1.M2_RUNNER)
    # The new process parser and original R4 dependencies are pinned imports.
    root = Path(args.campaign).resolve()
    sys.path.insert(0, str(root / 'tooling/tools/benchcampaign/multinode'))
    inputs.digest('tooling/tools/benchcampaign/multinode/node_sampler.py')
    inputs.digest('tooling/tools/benchcampaign/multinode/mn_common.py')
    import node_sampler
    need(Path(node_sampler.__file__).resolve() == inputs.path('tooling/tools/benchcampaign/multinode/node_sampler.py'),
         'unexpected process parser')
    import helpers
    pinned_helpers = {}
    for module in (sys.modules[__name__], helpers, legacy_io, legacy_m1, m2_export, m2_export.old, server_cpu,
                   node_sampler, node_sampler.mc):
        path = Path(module.__file__).resolve()
        need(path.is_relative_to(root), 'execute only the staged pinned exporter/helpers')
        relative = path.relative_to(root).as_posix()
        pinned_helpers[relative] = inputs.digest(relative)
    result = export(inputs, args.analysis, client.bootstrap_log_ratio, testingb.paired_gm, node_sampler)
    result['method']['export_helpers_sha256'] = pinned_helpers
    files = {'results.json': json.dumps(result, indent=2, sort_keys=True, allow_nan=False) + '\n'}
    legacy_io.validate_public(files, inputs.load(args.redactions))
    inputs.verify_all()
    output = Path(args.output_dir)
    need(not output.exists(), 'output already exists')
    output.mkdir(parents=True)
    for name, text in files.items():
        (output / name).write_text(text)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
