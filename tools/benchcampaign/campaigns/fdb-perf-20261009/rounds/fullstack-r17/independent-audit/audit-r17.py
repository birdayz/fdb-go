#!/usr/bin/env python3
"""Independent offline R17 raw/math audit. No production analyzer code imported."""
import csv
import datetime
import hashlib
import itertools
import json
import math
from pathlib import Path
import random
import struct
import sys

D = Path('${FDB_PERF_HOME}/results/fdb-perf-20261009')
ROOT = D / 'fullstack-r17-extracted/fullstack-r17/artifacts/fullstack-r17-timing'
PUBLISHED = ROOT.parent / 'fullstack-r17-analysis'
OUT = Path(__file__).resolve().parent
ARCHIVE_SHA = '4943768b532b2c57797c677f37b9381c10d922fbee81ef7821a3174a29d2e6f0'
BINARIES = {'new': '93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f',
            'cgo': '4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c'}
ENDPOINTS = [('batchset50', 'shutdown_process_cpu_ns_per_op'), ('batchset50', 'process_cpu_ns_per_op'),
             ('read1000', 'mean_ns'), ('read1000', 'p99_ns'), ('read1000', 'shutdown_process_cpu_ns_per_op')]
SNAPSHOT_FIELDS = {'process_user_cpu_us', 'process_system_cpu_us', 'process_high_water_rss_kb',
                   'go_total_alloc_bytes', 'go_mallocs', 'go_gc_cycles', 'go_gc_pause_ns'}


def need(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(path.read_text())


def sha(path):
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def dump(name, obj):
    (OUT / name).write_text(json.dumps(obj, indent=2, sort_keys=True, allow_nan=False) + '\n')


def csv_write(name, rows):
    with (OUT / name).open('w', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)


def expected_order(segment):
    rng = random.Random(797001 + segment)
    permutations = list(itertools.permutations(('new', 'cgo')))
    orders = {}
    for workload in ('read1000', 'batchset50'):
        choices = permutations[:]
        rng.shuffle(choices)
        orders[workload] = choices
    result = []
    for block in range(2):
        workloads = ['read1000', 'batchset50']
        rng.shuffle(workloads)
        for workload in workloads:
            result.extend((workload, block, position, arm) for position, arm in enumerate(orders[workload][block]))
    return result


def samples(path, count, phase):
    latencies = []
    with path.open(newline='') as stream:
        reader = csv.DictReader(stream)
        for index, row in enumerate(reader):
            need(row['phase'] == phase and row['index'] == str(index) and row['worker'] == '0', f'{path}: sample identity')
            need(row['attempts'] == '1' and row['error'] == '' and row['check_status'] == 'ok', f'{path}: retry/error/data check')
            latency = int(row['latency_ns'])
            need(latency > 0, f'{path}: nonpositive latency')
            latencies.append(latency)
    need(len(latencies) == count, f'{path}: count mismatch')
    return latencies


def audit_raw():
    complete = read(ROOT / 'complete.json')
    execution = read(ROOT / 'execution-manifest.json')
    need(complete['completed_segments'] == 80 and complete['completed_runs'] == 640, 'incomplete root')
    need(complete['execution_manifest_sha256'] == sha(ROOT / 'execution-manifest.json'), 'root manifest hash')
    rows, provenance, prefixes = [], [], set()
    first_counts = {workload: {'new': 0, 'cgo': 0} for workload in ('read1000', 'batchset50')}
    for segment in range(80):
        name = f'seg{segment:03d}'
        folder = ROOT / name
        plan, schedule, finished = [read(folder / filename) for filename in ('plan.json', 'schedule.json', 'complete.json')]
        record = execution['segments'][segment]
        need(record['index'] == segment and record['global_block_offset'] == 2 * segment, 'global mapping')
        for label in ('plan', 'schedule'):
            need(sha(folder / (label + '.json')) == sha(ROOT / record[label]) == record[label + '_sha256'], 'prepin mismatch')
        need(plan['seed'] == 797001 + segment and plan['blocks'] == 2 and finished['completed'] == finished['scheduled'] == 8, 'segment counts/seed')
        need(plan['expected_sha256'] == finished['binaries'] == execution['input_sha256'], 'common identities mismatch')
        observed = [(entry['workload']['name'], entry['block'], entry['position'], entry['arm']) for entry in schedule]
        need(observed == expected_order(segment), f'{name}: independently regenerated randomization differs')
        for entry in schedule:
            raw = folder / entry['run_id']
            summary, manifest, result = [read(raw / filename) for filename in ('summary.json', 'manifest.json', 'result.json')]
            workload, arm = entry['workload']['name'], entry['arm']
            count = 5000 if workload == 'read1000' else 2500
            need(summary['config'] == manifest['config'] and summary['backend'] == manifest['backend'], 'summary/manifest mismatch')
            need(summary['backend']['executable_sha256'] == BINARIES[arm], 'frozen binary changed')
            need(summary['backend']['native_library'] == (arm == 'cgo') and manifest['gomaxprocs'] == 8, 'backend/runtime identity')
            need(summary['success'] is True and summary['persisted_validation'] == 'ok' and result['returncode'] == 0, 'failed run')
            need(summary['completed'] == summary['requested'] == count and summary['warmup_completed'] == summary['warmup_requested'] == 200, 'run transaction count')
            need(all(type(summary[key]) is int and summary[key] == 0 for key in ('errors', 'retries', 'warmup_errors', 'warmup_retries')), 'nonzero retry/error')
            prefix = summary['config']['prefix']
            need(prefix not in prefixes and len(prefix.encode()) == 68, 'prefix reuse/length')
            prefixes.add(prefix)
            life = summary['lifecycle']
            need(life['complete'] and life['database_closed'] and life['measurement_barrier_complete'] and life['network_stopped'] == (arm == 'cgo'), 'lifecycle incomplete')
            snapshots = [summary['stats_after_identity'], summary['stats_before'], summary['stats_after'], life['stats_after_measurement_drain'], life['stats_after_shutdown']]
            need(all(set(snapshot) == SNAPSHOT_FIELDS and all(type(v) is int and v >= 0 for v in snapshot.values()) for snapshot in snapshots), 'A-E integer schema')
            need(all(after[key] >= before[key] for before, after in zip(snapshots, snapshots[1:]) for key in SNAPSHOT_FIELDS), 'decreasing A-E counter')
            for label in ('measurement_drain', 'shutdown_drain'):
                drain = life[label]
                need(drain['complete'] and 2 <= drain['rounds'] <= 16, 'failed finalizer drain')
                need(drain['after']['finalizers_queued'] == drain['after']['finalizers_executed'], 'pending finalizers')
                need(drain['before']['cleanups_queued_estimate'] == drain['after']['cleanups_queued_estimate'] == drain['after']['cleanups_executed_estimate'], 'pending cleanups')
            latency = samples(raw / 'samples.csv', count, 'measure')
            samples(raw / 'warmup.csv', 200, 'warmup')
            A, B, C, _, E = snapshots
            cpu = lambda obj: obj['process_user_cpu_us'] + obj['process_system_cpu_us']
            window = (cpu(C) - cpu(B)) * 1000 / count
            shutdown = (cpu(E) - cpu(A)) * 1000 / count
            need(window > 0 and shutdown > 0, 'nonpositive primary CPU')
            # Integer arithmetic implements nearest-rank ceil(.99*N)-1 independently.
            p99_index = (99 * count + 99) // 100 - 1
            row = dict(run_id=f'{name}/{entry["run_id"]}', workload=workload, arm=arm, global_block=2 * segment + entry['block'],
                       count=count, mean_ns=sum(latency) / count, p99_ns=sorted(latency)[p99_index],
                       process_cpu_ns_per_op=window, shutdown_process_cpu_ns_per_op=shutdown)
            rows.append(row)
            if entry['position'] == 0:
                first_counts[workload][arm] += 1
            for filename in ('summary.json', 'manifest.json', 'result.json', 'samples.csv', 'warmup.csv'):
                path = raw / filename
                provenance.append(dict(path=str(path.relative_to(ROOT)), sha256=sha(path)))
    need(len(rows) == 640 and len(prefixes) == 640, 'run census mismatch')
    need(first_counts == {workload: {'new': 80, 'cgo': 80} for workload in first_counts}, 'arm order imbalance')
    by = {(row['workload'], row['global_block'], row['arm']): row for row in rows}
    need(len(by) == 640, 'duplicate paired identity')
    logs, paired = [], []
    for block in range(160):
        vector = []
        record = {'global_block': block}
        for column, (workload, metric) in enumerate(ENDPOINTS):
            n, c = by[workload, block, 'new'][metric], by[workload, block, 'cgo'][metric]
            ratio = n / c
            vector.append(math.log(ratio))
            record.update({f'endpoint_{column+1}_new': n, f'endpoint_{column+1}_cgo': c,
                           f'endpoint_{column+1}_ratio': ratio, f'endpoint_{column+1}_log_ratio': vector[-1]})
        logs.append(vector)
        paired.append(record)
    return rows, provenance, logs, paired, first_counts


def literal_bootstrap(logs, length, seed):
    rng = random.Random(seed)
    columns = list(zip(*logs))
    values = [[] for _ in ENDPOINTS]
    digest = hashlib.sha256()
    # Deliberately expand all160 indices every resample, unlike production's precomputed circular-window sums.
    for _ in range(100_000):
        starts = [rng.randrange(160) for _ in range(160 // length)]
        digest.update(bytes(starts))
        indices = [(start + offset) % 160 for start in starts for offset in range(length)]
        need(len(indices) == 160, 'wrong bootstrap population')
        for column, observations in enumerate(columns):
            values[column].append(math.exp(math.fsum(observations[index] for index in indices) / 160))
    result = dict(length=length, seed=seed, resamples=100_000, index_stream_sha256=digest.hexdigest(), intervals=[],
                  method='independent literal160-index expansion; one shared index vector for five endpoints',
                  nearest_rank_zero_based_indices=[499, 99499], distribution_sha256_float64_be_before_sort=[])
    for distribution in values:
        result['distribution_sha256_float64_be_before_sort'].append(hashlib.sha256(b''.join(struct.pack('!d', value) for value in distribution)).hexdigest())
        distribution.sort()
        result['intervals'].append(dict(ci99_low=distribution[499], ci99_high=distribution[99499]))
    return result


def main():
    try:
        need(sha(D / 'fullstack-r17.tar.gz') == ARCHIVE_SHA, 'sealed archive SHA mismatch')
        rows, provenance, logs, paired, first_counts = audit_raw()
        published = read(PUBLISHED / 'analysis.json')
        expected_pairs = published['primary_per_block']
        need(len(expected_pairs) == 160 and len(published['per_run']) == 640, 'published census mismatch')
        max_pair_error = 0.
        for actual, expected in zip(paired, expected_pairs):
            need(actual['global_block'] == expected['global_block'], 'published block order')
            for key in actual:
                if key == 'global_block':
                    continue
                difference = abs(actual[key] - expected[key])
                max_pair_error = max(max_pair_error, difference)
                need(actual[key] == expected[key], f'raw-to-published endpoint/log mismatch: block{actual["global_block"]} {key}')
        points = [math.exp(math.fsum(row[column] for row in logs) / 160) for column in range(5)]
        for point, expected in zip(points, published['primary_endpoints']):
            need(point == expected['geometric_mean_ratio'], 'GM mismatch')
        boots = [literal_bootstrap(logs, length, seed) for length, seed in ((8, 797707), (16, 797708))]
        comparison_rows = []
        maximum_ci_error = 0.
        for boot, expected in zip(boots, published['bootstrap']):
            need(boot['index_stream_sha256'] == expected['index_stream_sha256'], 'bootstrap index-stream mismatch')
            for column, (actual_ci, expected_ci) in enumerate(zip(boot['intervals'], expected['intervals'])):
                for side in ('ci99_low', 'ci99_high'):
                    error = abs(actual_ci[side] - expected_ci[side])
                    maximum_ci_error = max(maximum_ci_error, error)
                    need(math.isclose(actual_ci[side], expected_ci[side], rel_tol=1e-14, abs_tol=1e-15), f'bootstrap bound mismatch L{boot["length"]} endpoint{column+1}')
        for column, (workload, metric) in enumerate(ENDPOINTS):
            l8, l16 = [boot['intervals'][column] for boot in boots]
            decision = l8['ci99_high'] < 1 and l16['ci99_high'] < 1
            need(decision == published['primary_endpoints'][column]['improvement_confirmed'], 'published decision differs')
            comparison_rows.append(dict(endpoint=column+1, workload=workload, metric=metric, geometric_mean_ratio=points[column],
                                        L8_ci99_low=l8['ci99_low'], L8_ci99_high=l8['ci99_high'],
                                        L16_ci99_low=l16['ci99_low'], L16_ci99_high=l16['ci99_high'], improvement_confirmed=decision))
        outcome = dict(status='passed', independent_of_production_code=True, production_analyzer_imported=False,
                       archive_sha256=ARCHIVE_SHA, raw_root=str(ROOT), published_root=str(PUBLISHED),
                       valid_raw_runs=640, paired_blocks_per_workload=160, first_arm_counts=first_counts,
                       primary_log_vectors=160, primary_log_columns=5, raw_files_hashed=len(provenance),
                       raw_to_published_value_and_log_max_absolute_error=max_pair_error,
                       bootstrap_bound_max_absolute_error=maximum_ci_error,
                       bootstrap_note='Literal expanded-index fsum can differ at last floating-point bit from nested precomputed window fsum; comparisons use1e-14 relative/1e-15 absolute tolerance.',
                       bootstrap=boots, endpoints=comparison_rows, code_sha256=sha(Path(__file__)), python_version=sys.version,
                       sealed_files_modified=False, host_gate_scope='This independently audits raw samples, identity/lifecycle counts, schedule pairing and primary math; it does not repeat all host telemetry/quiescence validation or all27153 archive member checks.')
        dump('audit.json', outcome)
        csv_write('recomputed-per-run.csv', rows)
        csv_write('recomputed-primary-per-block.csv', paired)
        csv_write('recomputed-primary-decisions.csv', comparison_rows)
        csv_write('raw-files-read.csv', provenance)
        text = ['# Independent R17 raw/math audit\n',
                'PASS:640 raw runs,160 paired blocks/workload, all800 primary paired logs and all five GMs exactly match published values. '
                'Both100000-resample joint circular bootstraps were recomputed with literal expanded160-index draws, not production window-sum code.\n',
                f'Maximum CI endpoint absolute difference: {maximum_ci_error:.17g}. Fixed index-stream hashes match exactly.\n',
                '|Endpoint|Metric|GM NEW/CGo|L8 99%CI|L16 99%CI|Both upper<1|\n|---|---|---:|---|---|---|\n']
        for row in comparison_rows:
            text.append(f"|{row['endpoint']}|{row['workload']} {row['metric']}|{row['geometric_mean_ratio']:.9f}|"
                        f"[{row['L8_ci99_low']:.9f},{row['L8_ci99_high']:.9f}]|[{row['L16_ci99_low']:.9f},{row['L16_ci99_high']:.9f}]|{row['improvement_confirmed']}|\n")
        text.append('\nNo sealed files were modified. The whole archive digest was independently verified;3200 directly used raw files are inventoried. '
                    'This is not a repetition of all host gates or all27153 archived-file checks. No new fleet, causal, or correctness claim is added.\n')
        (OUT / 'AUDIT.md').write_text('\n'.join(text))
        print('\n'.join(text))
    except Exception as error:
        dump('audit.json', dict(status='failed', error=str(error), error_type=type(error).__name__, sealed_files_modified=False))
        raise


if __name__ == '__main__':
    main()
