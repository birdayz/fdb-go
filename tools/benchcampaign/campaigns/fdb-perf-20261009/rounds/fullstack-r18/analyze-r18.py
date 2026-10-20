#!/usr/bin/env python3
"""Offline R18 descriptive analysis: 18 uninstrumented runs plus two separate traces.
Usage: python3 -B analyze-r18.py INPUT_ROOT NEW_OUTPUT_DIR [--v7-scripts DIR]
No confidence intervals, tests of effects, acquisition, or input modifications.
"""
import argparse
import csv
import datetime
import hashlib
import importlib.util
import itertools
import json
import math
from pathlib import Path
import shutil
import statistics
import sys

sys.dont_write_bytecode = True
BASE = '7922a2e5a8e3a2d6cf17dfee8b8134181c88b298'
PATCH = 'd0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85'
HARNESS = '48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203'
LIBRARY = 'e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1'
SERVER = '179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f'
GO_BINARY = '93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f'
CGO_BINARY = '4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c'
TRACE_PARSER = '9bafafddd9f8f7dfd9464283265512933d6e811be09130769bb987860a93f1c0'
REGISTRATION = '8fb5c623fc8dbe81d71e6cada2cf66ac2292362464466543e95650c52b229698'
FROZEN = {'run.py': '7ce9e96c4e736b2df559f62b00c4eacfd4ad4b1623ca380d94e07896d96669f2',
          'analyze.py': '5d3086d28e7e856f4b9f9033e6f6790e1e90031fa2efe19b122b27b731fe577b',
          'r3_plan.py': 'effa82052b4327d0998b4c914250f4bd1e66e22397003b818d55f4892904f9d3'}
ARMS = ('go8', 'go2', 'cgo')
PAIRS = (('go2', 'go8'), ('go8', 'cgo'), ('go2', 'cgo'))
ENV = {'GOMAXPROCS': '8', 'GOGC': '100', 'GOMEMLIMIT': 'off', 'GODEBUG': '', 'LC_ALL': 'C', 'TZ': 'UTC'}
SCOPES = {
    'window': '(C-B)/N: measured-window process CPU, not complete lifecycle; deferred cleanup may cross boundaries',
    'drained': '(D-B)/N: through finalizer fixpoint/native FIFO barrier; includes forced GC and pending setup/warmup cleanup',
    'shutdown': '(E-A)/N: includes startup, fixture, warmup, validation, CSV, forced GC and shutdown; excludes prior identity hashing, final JSON and process exit; NOT isolated steady-state CPU',
    'absolute_E': 'E/N: absolute/unpaired only; includes identity hashing; NEVER paired cross-arm client-cost evidence',
}
METRICS = ('mean_ns', 'p99_ns', 'window_cpu_ns_per_op', 'window_user_cpu_ns_per_op', 'window_system_cpu_ns_per_op',
           'drained_cpu_ns_per_op', 'drained_user_cpu_ns_per_op', 'drained_system_cpu_ns_per_op',
           'shutdown_cpu_ns_per_op', 'shutdown_user_cpu_ns_per_op', 'shutdown_system_cpu_ns_per_op',
           'go_alloc_bytes_per_op', 'go_mallocs_per_op')
CPU_SETS = [dict(name='housekeeping', cpus=[0, 1], max_busy_fraction=.3, max_cpu_busy_fraction=.8),
            dict(name='server', cpus=list(range(2, 8)), max_busy_fraction=.15, max_cpu_busy_fraction=.5),
            dict(name='client', cpus=list(range(8, 16)), max_busy_fraction=.05, max_cpu_busy_fraction=.15)]


def need(condition, message):
    if not condition:
        raise ValueError(message)


def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(data)
    return h.hexdigest()


def read(path):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            need(key not in result, f'{path}: duplicate JSON key {key}')
            result[key] = value
        return result
    def invalid(text):
        raise ValueError(f'{path}: nonfinite JSON {text}')
    return json.loads(path.read_text(), object_pairs_hook=unique, parse_constant=invalid)


def dump(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + '\n')


def csv_out(path, rows, fields=None):
    path.parent.mkdir(parents=True, exist_ok=True)
    fields = fields or list(dict.fromkeys(key for row in rows for key in row))
    need(bool(fields), f'no CSV schema: {path}')
    with path.open('w', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows({key: json.dumps(value, sort_keys=True) if isinstance(value, (list, dict)) else value
                         for key, value in row.items()} for row in rows)


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def load_helpers(folder):
    for name, sha in FROZEN.items():
        need(digest(folder / name) == sha, f'frozen V7 script changed: {name}')
    path = Path(__file__).parent / 'analysis-support' / 'r16_trace_parser.py'
    need(digest(path) == TRACE_PARSER, 'audited R16 trace parser changed')
    return (load_module('run', folder / 'run.py'), load_module('r18_v7_analyze', folder / 'analyze.py'),
            load_module('r18_trace_parser', path))


def relative(root, name):
    need(isinstance(name, str) and not Path(name).is_absolute(), f'invalid relative path {name}')
    path = (root / name).resolve(strict=True)
    need(path.is_relative_to(root), f'path escapes root: {name}')
    return path


def utc(value):
    result = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
    need(result.tzinfo is not None, 'UTC timezone missing')
    return result


def inventory(root):
    result = {}
    for path in sorted(root.rglob('*')):
        need(not path.is_symlink() and (path.is_dir() or path.is_file()), f'special/symlink input: {path}')
        if path.is_file():
            result[str(path.relative_to(root))] = {'sha256': digest(path), 'bytes': path.stat().st_size}
    return result


def arm_environment(arm):
    return dict(ENV, GOMAXPROCS='2' if arm == 'go2' else '8')


def check_plan(plan, pins, arm=None, trace=False):
    need(plan['expected_sha256'] == pins, 'plan common pins differ')
    for key, value in [('source_commit', BASE), ('harness_id', HARNESS), ('native_library_sha256', LIBRARY),
                       ('fdb_api_version', 730), ('fdb_version', '7.3.77'), ('go_version', 'go1.26.9'),
                       ('cooldown_seconds', 2), ('quiescence_seconds', 1), ('telemetry_seconds', 1),
                       ('max_busy_fraction', .15), ('max_steal_fraction', .01), ('cpu_sets', CPU_SETS),
                       ('timeout_seconds', 180), ('campaign_wall_cap_seconds', 180)]:
        need(plan.get(key) == value, f'non-frozen plan.{key}')
    need(plan['environment'] == arm_environment(arm), 'environment differs from declared arm')
    need([a['name'] for a in plan['arms']] == list(ARMS), 'arm input order differs')
    for a in plan['arms']:
        native = a['name'] == 'cgo'
        expected_source = '8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead' if native else BASE + '.patch.' + PATCH
        need(a['source_revision'] == expected_source and pins[a['binary']] == (CGO_BINARY if native else GO_BINARY), 'arm binary/source changed')
    need(plan['arms'][0]['binary'] == plan['arms'][1]['binary'], 'Go arms must use identical executable path')
    for sha in (HARNESS, LIBRARY, SERVER, PATCH, *FROZEN.values(), TRACE_PARSER):
        need(sha in pins.values(), f'missing frozen identity pin {sha}')
    if not trace:
        need(plan['blocks'] == 6 and plan['seed'] == 798001, 'global scheduler seed/block count changed')
        need(plan['workloads'] == [dict(name='read1000', iterations=5000, warmup=200, workers=1)], 'timing workload changed')


def check_quiescence(folder, plan, runner):
    gate = read(folder / 'quiescence.json')
    need(gate.get('passed') is True and gate['seconds'] == 1 and gate['max_steal_fraction'] == .01, 'failed pre-run quiescence')
    cpus = gate['cpus']
    need(set(cpus) == {'cpu', *(f'cpu{i}' for i in range(16))}, 'quiescence CPU population differs')
    for key, row in cpus.items():
        need(runner.cpu_usage(row['cpu_jiffy_deltas']) == row, f'quiescence arithmetic mismatch {key}')
    for group in plan['cpu_sets']:
        members = [cpus[f'cpu{cpu}'] for cpu in group['cpus']]
        aggregate = runner.cpu_usage([sum(row['cpu_jiffy_deltas'][i] for row in members) for i in range(8)])
        maximum = max(row['busy_fraction'] for row in members)
        passed = (aggregate['busy_fraction'] <= group['max_busy_fraction'] and maximum <= group['max_cpu_busy_fraction']
                  and max(row['steal_fraction'] for row in members) <= plan['max_steal_fraction'])
        need(passed and gate['groups'][group['name']] == dict(aggregate, maximum_cpu_busy_fraction=maximum, thresholds=group, passed=True), 'quiescence group mismatch/failure')
    need(cpus['cpu']['busy_fraction'] <= .15, 'global quiescence failed')
    return gate


def check_run(folder, entry, plan, pins, runner, count):
    need(entry['arm'] in ARMS and entry['workload'] == dict(name='read1000', iterations=count, warmup=200, workers=1), 'run workload/arm changed')
    runner.validate_raw_output(folder, entry['workload'])
    runner.validate_identity(folder, entry, plan, pins)
    summary, manifest, result = [read(folder / name) for name in ('summary.json', 'manifest.json', 'result.json')]
    need(type(result['returncode']) is int and result['returncode'] == 0 and result['run_id'] == entry['run_id'], 'failed/mismatched result')
    need(utc(result['started_utc']) < utc(result['ended_utc']), 'reversed process timestamps')
    need(manifest['environment'] == {key: arm_environment(entry['arm'])[key] for key in ('GOMAXPROCS', 'GOGC', 'GOMEMLIMIT', 'GODEBUG')}, 'observed environment differs')
    for field in ('errors', 'warmup_errors', 'retries', 'warmup_retries'):
        need(type(summary[field]) is int and summary[field] == 0, f'nonzero/invalid {field}')
    need(summary['failure'] == '' and summary['warmup_requested'] == 200, 'failure/warmup mismatch')
    latencies = []
    for filename, n in [('samples.csv', count), ('warmup.csv', 200)]:
        with (folder / filename).open(newline='') as stream:
            rows = list(csv.DictReader(stream))
        need(len(rows) == n and all(row['attempts'] == '1' for row in rows), 'retry or sample count mismatch')
        if filename == 'samples.csv':
            latencies = [int(row['latency_ns']) for row in rows]
    if entry['arm'] != 'cgo':
        counters = summary['client_counters_delta']['values']
        need(all(type(counters[k]) is int and counters[k] == 0 for k in ('transaction_retries', 'connection_failures', 'grv_cache_hits', 'commit_started', 'commit_completed')), 'Go counter failure')
        need(counters['read_versions_completed'] == counters['grv_successful_batch_replies'] == count, 'Go GRV count differs')
    command = entry['command']
    need(command[:3] == ['taskset', '--cpu-list', '8-15'], 'benchmark affinity differs')
    for flag, expected in [('--transaction-timeout-ms', '30000'), ('--retry-limit', '10')]:
        need(command.count(flag) == 1 and command[command.index(flag) + 1] == expected, f'benchmark {flag} differs')
    prefix = command[command.index('--prefix') + 1]
    need(len(prefix.encode()) == 68, 'prefix length differs')
    gate = check_quiescence(folder, plan, runner)
    telemetry = (folder / 'telemetry.jsonl').read_text().splitlines()
    need(telemetry, 'missing raw telemetry')
    for line in telemetry:
        need(isinstance(json.loads(line), dict), 'invalid raw telemetry record')
    return dict(folder=folder, entry=entry, summary=summary, manifest=manifest, result=result, latencies=latencies,
                prefix=prefix, quiescence=gate, telemetry_records=len(telemetry))


def run_statistics(item, root, hashes, analyzer):
    summary, entry = item['summary'], item['entry']
    life = summary['lifecycle']
    snapshots = [summary['stats_after_identity'], summary['stats_before'], summary['stats_after'], life['stats_after_measurement_drain'], life['stats_after_shutdown']]
    A, B, C, D, E = snapshots
    n = len(item['latencies'])
    need(n == 5000, 'instrumented/short run cannot enter timing statistics')
    relative_path = str(item['folder'].relative_to(root))
    row = dict(run_id=relative_path, local_run_id=entry['run_id'], block=entry['block'], position=entry['position'], arm=entry['arm'],
               gomaxprocs=item['manifest']['gomaxprocs'], observations=n, warmup_observations=200,
               mean_ns=statistics.mean(item['latencies']), p99_ns=analyzer.quantile(item['latencies'], .99),
               absolute_E_cpu_ns_per_op=(E['process_user_cpu_us'] + E['process_system_cpu_us']) * 1000 / n,
               go_alloc_bytes_per_op=(C['go_total_alloc_bytes'] - B['go_total_alloc_bytes']) / n,
               go_mallocs_per_op=(C['go_mallocs'] - B['go_mallocs']) / n,
               result=item['result'], quiescence=item['quiescence'], telemetry_records=item['telemetry_records'],
               measured_wall_ns=summary['measured_wall_ns'], lifecycle=life, backend=item['manifest']['backend'], config=summary['config'],
               client_counters_delta=summary['client_counters_delta'],
               raw_files={name[len(relative_path)+1:]: data for name, data in hashes.items() if name.startswith(relative_path + '/')})
    for label, start, end in [('window', B, C), ('drained', B, D), ('shutdown', A, E)]:
        user = (end['process_user_cpu_us'] - start['process_user_cpu_us']) * 1000
        system = (end['process_system_cpu_us'] - start['process_system_cpu_us']) * 1000
        need(user >= 0 and system >= 0 and user + system > 0, 'invalid CPU interval')
        row.update({label + '_cpu_ns_per_op': (user + system) / n, label + '_user_cpu_ns_per_op': user / n,
                    label + '_system_cpu_ns_per_op': system / n, label + '_user_cpu_ns': user, label + '_system_cpu_ns': system,
                    label + '_cpu_scope': SCOPES[label]})
    for label, snapshot in zip('ABCDE', snapshots):
        row.update({label + '_' + key: value for key, value in snapshot.items()})
    return row


def descriptive(rows):
    need(len(rows) == 18, 'exactly eighteen timing observations required')
    by = {(row['block'], row['arm']): row for row in rows}
    need(set(by) == {(block, arm) for block in range(6) for arm in ARMS}, 'missing/duplicate timing pair keys')
    paired, summaries, absolute = [], [], []
    for numerator, denominator in PAIRS:
        for metric in METRICS:
            values = []
            for block in range(6):
                n, d = by[block, numerator][metric], by[block, denominator][metric]
                need(math.isfinite(n) and math.isfinite(d) and n >= 0 and d >= 0, 'invalid timing statistic')
                ratio = n / d if n > 0 and d > 0 else None
                values.append(ratio)
                paired.append(dict(block=block, numerator=numerator, denominator=denominator, metric=metric,
                                   numerator_value=n, denominator_value=d, ratio=ratio,
                                   numerator_run_id=by[block, numerator]['run_id'], denominator_run_id=by[block, denominator]['run_id']))
            complete = all(value is not None for value in values)
            summaries.append(dict(numerator=numerator, denominator=denominator, metric=metric, blocks=6, ratios_in_block_order=values,
                                  geometric_mean_ratio=math.exp(math.fsum(math.log(value) for value in values) / 6) if complete else None,
                                  minimum_ratio=min(values) if complete else None, maximum_ratio=max(values) if complete else None,
                                  interpretation='descriptive only; no CI, test, equivalence, or shipping recommendation' if complete else 'log ratio undefined due to zero; no blocks excluded'))
    for arm in ARMS:
        selected = [by[block, arm] for block in range(6)]
        for metric in (*METRICS, 'absolute_E_cpu_ns_per_op'):
            values = [row[metric] for row in selected]
            absolute.append(dict(arm=arm, metric=metric, values_in_block_order=values, median=statistics.median(values),
                                 minimum=min(values), maximum=max(values), paired_allowed=metric != 'absolute_E_cpu_ns_per_op'))
    return paired, summaries, absolute


def strip_whole_normalization(data):
    whole = data['whole_short_process']
    whole.pop('wholeprocess_calls_divided_by_200', None)
    for counts in [whole['aggregate'], *whole['per_tid'].values()]:
        for row in counts.values():
            row.pop('wholeprocess_calls_divided_by_200', None)
    data['whole_short_process_scope'] = 'Whole short-process selected syscall ENTRY counts, including startup/identity/fixture/warmup/drains/validation/shutdown; NOT phase counts; no per-transaction normalization'
    return data


def trace_outputs(out, arm, data, timeline):
    dest = out / 'strace-sequence' / arm
    dump(dest / 'analysis.json', data)
    dump(dest / 'markers.json', dict(markers=data['markers'], window=data['window'], samples=data['samples']))
    dump(dest / 'measured-window-timeline.json', dict(scope=data['measured_window_scope'], events=timeline))
    dump(dest / 'boundary-overlaps.json', data['boundary_overlap_events'])
    for population, name in [('whole_short_process', 'whole-process-counts.csv'), ('measured_window', 'measured-window-counts.csv')]:
        counts = []
        for tid, syscalls in [('ALL', data[population]['aggregate']), *data[population]['per_tid'].items()]:
            scope = data['measured_window_scope'] if population == 'measured_window' else data['whole_short_process_scope']
            counts += [dict(tid=tid, syscall=syscall, population=population, scope=scope, **values) for syscall, values in syscalls.items()]
        csv_out(dest / name, counts)
    fields = ['entry_epoch', 'end_epoch', 'tid', 'syscall', 'arguments', 'result', 'entry_in_measured_window',
              'boundary_overlap', 'marker', 'completion_known', 'duration_ns', 'source_file', 'source_lines']
    csv_out(dest / 'measured-window-timeline.csv', [{key: event.get(key) for key in fields} for event in timeline], fields)
    lines = ['# Diagnostic syscall timeline ONLY; no timing/CPU comparison or exact transaction alignment.',
             '# Includes flagged carry-in/carry-out and B/C reference markers; only entry_in_measured_window rows count.']
    for event in timeline:
        lines.append(f"{event['entry_epoch']} [tid {event['tid']}] {event['syscall']}({event['arguments']}) = {event['result']} "
                     f"[end={event['end_epoch']} entry_count={event['entry_in_measured_window']} overlap={event['boundary_overlap']} "
                     f"marker={event.get('marker')} raw={event['source_file']}:{event['source_lines']}]")
    (dest / 'measured-window-timeline.txt').write_text('\n'.join(lines) + '\n')


def tables(comparisons, absolute, traces):
    text = ['# R18 exploratory GOMAXPROCS diagnosis\n',
            '18 uninstrumented processes; six blocks with all six arm orders, fixed seed798001. '
            'go8/go2 are the SAME frozen NEW executable with GOMAXPROCS=8/2; CGo remains8. '
            'Clients retain CPUs8–15. No confidence intervals, hypothesis tests, equivalence, sample-size extension, '
            'or threshold-selected shipping decision. No R17 observations pooled; these diagnostics cannot rescue an R17 endpoint.\n',
            'Paired GM is exp(mean of six within-block log ratios), NOT a ratio of medians. '
            'Ranges describe the six observed ratios, NOT confidence bounds.\n',
            '|Ratio|Metric|GM|Observed ratio range|Six ratios in block order|\n|---|---|---:|---|---|\n']
    for row in comparisons:
        values = ', '.join('undefined' if value is None else f'{value:.6f}' for value in row['ratios_in_block_order'])
        text.append(f"|{row['numerator']}/{row['denominator']}|{row['metric']}|{row['geometric_mean_ratio']}|{row['minimum_ratio']}–{row['maximum_ratio']}|{values}|\n")
    text += ['\n## Accounting\n', *[f'- {name}: {scope}.\n' for name, scope in SCOPES.items()],
             '\nGo allocation counters exclude the native C++ heap. CPU/latency values are ns/transaction. '
             'Raw A–E integer snapshots, per-run values and all paired values are in CSV/JSON. Absolute E/N is unpaired only.\n',
             '\n## Absolute descriptive statistics\n\n|Arm|Metric|Median of six run statistics|Observed minimum|Observed maximum|\n|---|---|---:|---:|---:|\n']
    for row in absolute:
        text.append(f"|{row['arm']}|{row['metric']}|{row['median']}|{row['minimum']}|{row['maximum']}|\n")
    text.append('\n## Separate trace population — NEVER timing evidence\n\nEach trace is200 measured+200warmup, after all18 timings. '
                'Exact unique chronological A–E CPU-us/RSS signatures define [B return,C entry). Counts are syscall ENTRY counts, '
                'not completions or syscall-duration apportionment. Only phase counts are divided by200. '
                'samples.csv has a relative epoch just after B, with unknown submillisecond offset: no exact transaction alignment. '
                'Instrumentation perturbs scheduling; no CPU/latency improvement or causal attribution follows from these traces.\n')
    for arm, data in traces.items():
        text.append(f"\n{arm}: whole selected entries={data['whole_short_process']['total_entries']}; phase entries={data['measured_window']['total_entries']}; "
                    f"thread-name observation records={data['thread_name_observations']['records']}. See per-TID count tables, marker evidence and merged timeline.\n")
    text.append('\n|Syscall|go8 whole entries|go2 whole entries|go8 phase entries|go2 phase entries|go8 phase/200|go2 phase/200|\n|---|---:|---:|---:|---:|---:|---:|\n')
    for syscall in sorted(traces['go8']['measured_window']['aggregate']):
        whole = [traces[arm]['whole_short_process']['aggregate'][syscall]['calls'] for arm in ('go8', 'go2')]
        phase = [traces[arm]['measured_window']['aggregate'][syscall]['calls'] for arm in ('go8', 'go2')]
        text.append(f'|{syscall}|{whole[0]}|{whole[1]}|{phase[0]}|{phase[1]}|{phase[0]/200:.6f}|{phase[1]/200:.6f}|\n')
    text.append('\nOne reimaged existing instance/fresh cluster, not independent host replication. '
                'Discrete raw telemetry is descriptive; no new during-run rejection thresholds. '
                'Benchmark diagnostics are not source/review/CI correctness approval or merge permission.\n')
    return '\n'.join(text)


def analyze(root, out, runner, analyzer, trace_parser):
    execution = read(root / 'execution-manifest.json')
    complete = read(root / 'complete.json')
    timing_complete = read(root / 'timing-complete.json')
    need(execution['schema'] == complete['schema'] == 'r18-diagnostics-v1', 'root schema differs')
    manifest_sha = digest(root / 'execution-manifest.json')
    need(complete['execution_manifest_sha256'] == manifest_sha, 'manifest completion seal mismatch')
    need(complete['completed_timing_runs'] == complete['scheduled_timing_runs'] == 18
         and complete['completed_trace_runs'] == complete['scheduled_trace_runs'] == 2, 'incomplete diagnostic campaign')
    need(timing_complete['completed_timing_runs'] == timing_complete['scheduled_timing_runs'] == 18
         and timing_complete['execution_manifest_sha256'] == manifest_sha, 'timing completion seal mismatch')
    pins = execution['input_sha256']
    for path in (Path(__file__), Path(__file__).parent / 'analysis-support' / 'r16_trace_parser.py'):
        need(digest(path) in pins.values(), f'analyzer implementation missing from pre-acquisition pins: {path.name}')
    registration = root / 'R18-preregistration-v1.md'
    need(digest(registration) == execution['preregistration_sha256'] == REGISTRATION and REGISTRATION in pins.values(), 'registration hash mismatch/missing pin')
    need(execution['timing_trace_cap_seconds'] == 720 and execution['per_process_cap_seconds'] == 180, 'registered caps differ')
    need(execution['global_plan'] == 'global-plan.json' and execution['global_schedule'] == 'global-schedule.json', 'global paths differ')
    for label in ('global_plan', 'global_schedule'):
        need(digest(relative(root, execution[label])) == execution[label + '_sha256'], 'global plan/schedule prepin mismatch')
    global_plan, schedule = read(root / 'global-plan.json'), read(root / 'global-schedule.json')
    check_plan(global_plan, pins)
    need(len(schedule) == 18 and len(execution['entries']) == 18, 'global schedule must have18 entries')
    need(analyzer.normalized_schedule(schedule) == analyzer.normalized_schedule(runner.make_schedule(global_plan, root / 'UNRESOLVED')), 'global frozen scheduler mismatch')
    need({tuple(run['arm'] for run in schedule if run['block'] == block) for block in range(6)} == set(itertools.permutations(ARMS)), 'not all six arm orders')
    need({path.name for path in root.glob('run*') if path.is_dir()} == {f'run{i:03d}' for i in range(18)}, 'extra/missing timing directories')
    hashes = inventory(root)
    need(not any(Path(path).name == 'ABORTED.json' for path in hashes), 'aborted acquisition')
    validated, prefixes, previous_end = [], set(), None
    for index, (record, entry) in enumerate(zip(execution['entries'], schedule)):
        need(record['index'] == index and record['name'] == f'run{index:03d}', 'timing index/name mismatch')
        folder = root / record['name']
        for label in ('plan', 'schedule'):
            need(digest(relative(root, record[label])) == record[label + '_sha256'], 'per-entry prepin mismatch')
            need(digest(folder / (label + '.json')) == record[label + '_sha256'], 'execute output differs from prepin bytes')
        plan = read(folder / 'plan.json')
        expected_plan = dict(global_plan, environment=arm_environment(entry['arm']), selected_run_index=index)
        need(plan == expected_plan, 'per-entry plan may change only GOMAXPROCS and record selected_run_index')
        check_plan(plan, pins, entry['arm'])
        need(read(folder / 'schedule.json') == [entry], 'singleton schedule differs from resolved global entry')
        need(read(folder / 'binaries.json') == read(folder / 'complete.json')['binaries'] == pins, 'entry input identity drift')
        done = read(folder / 'complete.json')
        need(done['scheduled'] == done['completed'] == 1, 'singleton execution incomplete')
        controller = read(folder / 'controller.json')
        need(controller['affinity'] == [0, 1] and controller['runner_sha256'] == FROZEN['run.py']
             and controller['plan_sha256'] == record['plan_sha256'], 'controller/plan/runner identity differs')
        environment = read(folder / 'benchmark-environment.json')
        need(set(environment) == set(ENV) | {'HOME', 'PATH'} and all(environment[k] == v for k, v in arm_environment(entry['arm']).items()), 'nonclean environment')
        raw = folder / entry['run_id']
        need(read(raw / 'invocation.json') == entry, 'timing invocation mismatch')
        item = check_run(raw, entry, plan, pins, runner, 5000)
        need(read(folder / 'progress.json') == [item['result']], 'singleton progress mismatch')
        start, end = utc(item['result']['started_utc']), utc(item['result']['ended_utc'])
        need(previous_end is None or previous_end <= start, 'timing processes overlap/out of order')
        need(utc(controller['utc']) <= start < end <= utc(done['utc']), 'singleton time boundaries inconsistent')
        need((utc(done['utc']) - utc(controller['utc'])).total_seconds() <= 180, 'singleton runner cap exceeded')
        previous_end = end
        need(item['prefix'] not in prefixes, 'reused prefix')
        prefixes.add(item['prefix'])
        validated.append(item)
    need(utc(timing_complete['finished_utc']) >= previous_end, 'timing seal predates last timing process')
    trace_data, timelines = {}, {}
    need([record['name'] for record in execution['traces']] == ['go8', 'go2'], 'trace order/population differs')
    need(read(root / 'strace-sequence' / 'complete.json')['completed'] == 2, 'trace completion seal missing')
    for trace_record in execution['traces']:
        arm = trace_record['name']
        folder = root / 'strace-sequence' / arm
        invocation, plan = read(folder / 'invocation.json'), read(folder / 'plan.json')
        entry = invocation['entry']
        for label in ('plan', 'schedule'):
            need(digest(relative(root, trace_record[label])) == trace_record[label + '_sha256'], 'trace prepin hash mismatch')
        need(digest(folder / 'plan.json') == trace_record['plan_sha256'], 'actual trace plan differs from prepin')
        need(read(relative(root, trace_record['schedule'])) == [entry] and invocation['command'] == trace_record['command'], 'trace invocation differs from prepin')
        need(entry['arm'] == arm, 'trace arm mismatch')
        check_plan(plan, pins, arm, trace=True)
        item = check_run(folder, entry, plan, pins, runner, 200)
        need(item['prefix'] not in prefixes, 'trace reused a timing/trace prefix')
        prefixes.add(item['prefix'])
        start, end = utc(item['result']['started_utc']), utc(item['result']['ended_utc'])
        need(start >= previous_end and start >= utc(timing_complete['finished_utc']), 'trace overlaps timing or prior trace')
        previous_end = end
        command = invocation['command']
        need(all(flag in command for flag in ('-ff', '-ttt', '-T', '-v', '-s', '-e', '-o')), 'trace flags missing')
        need(command[command.index('-s') + 1] == '128', 'trace string length differs')
        need(command[command.index('-e') + 1] == 'trace=' + ','.join(trace_parser.FILTER), 'trace syscall filter differs')
        need(command[-len(entry['command']):] == entry['command'], 'trace benchmark command differs')
        data, timeline = trace_parser.parse_sequence(folder)
        data = strip_whole_normalization(data)
        thread_names = folder / 'thread-names.jsonl'
        need(thread_names.is_file(), 'thread-name observation file missing')
        observations = [json.loads(line) for line in thread_names.read_text().splitlines() if line.strip()]
        names_by_tid = {}
        for observation in observations:
            need(observation['pid'] == item['manifest']['pid'] and isinstance(observation['names'], dict), 'thread-name PID/schema mismatch')
            for tid, name in observation['names'].items():
                need(str(tid).isdigit() and isinstance(name, str), 'invalid observed thread name/TID')
                names_by_tid.setdefault(tid, set()).add(name)
        data['thread_name_observations'] = dict(raw_path=f'raw/strace-sequence/{arm}/thread-names.jsonl', records=len(observations),
                                               names_by_tid={tid: sorted(names) for tid, names in names_by_tid.items()},
                                               status='observed comm names, not inferred runtime roles' if observations else 'no observations captured; do not infer thread names/roles')
        data['instrumented_timing_usable_for_performance_claims'] = False
        trace_data[arm], timelines[arm] = data, timeline
    need(utc(complete['finished_utc']) >= previous_end, 'root completion precedes final trace')
    started = read(root / 'STARTED.json')
    need(started['execution_manifest_sha256'] == manifest_sha, 'start seal differs')
    need((previous_end - utc(started['started_utc'])).total_seconds() <= 720, 'timing-plus-trace hard budget exceeded')
    need({path for path in hashes if path.endswith('/summary.json')} ==
         {str(item['folder'].relative_to(root) / 'summary.json') for item in validated} |
         {f'strace-sequence/{arm}/summary.json' for arm in ('go8', 'go2')}, 'unexpected extra/missing run summary')
    # No performance estimates until all timing and trace validity checks pass.
    rows = [run_statistics(item, root, hashes, analyzer) for item in validated]
    paired, comparisons, absolute = descriptive(rows)
    phase, whole = [], []
    for syscall in trace_parser.FILTER:
        whole.append(dict(syscall=syscall, **{arm + '_whole_process_entries': trace_data[arm]['whole_short_process']['aggregate'][syscall]['calls']
                                              for arm in ('go8', 'go2')}))
        counts = {arm: trace_data[arm]['measured_window']['aggregate'][syscall]['calls'] for arm in ('go8', 'go2')}
        phase.append(dict(syscall=syscall, go8_phase_entries=counts['go8'], go2_phase_entries=counts['go2'],
                          go8_phase_entries_divided_by_200=counts['go8']/200, go2_phase_entries_divided_by_200=counts['go2']/200))
    need(inventory(root) == hashes, 'raw inputs changed during analysis')
    need(digest(Path(__file__)) in pins.values() and digest(Path(__file__).parent / 'analysis-support' / 'r16_trace_parser.py') == TRACE_PARSER,
         'analysis implementation changed during execution')
    shutil.copytree(root, out / 'raw')
    need(inventory(out / 'raw') == hashes, 'raw preservation copy differs')
    report = dict(schema='r18-descriptive-analysis-v1', timing_runs=18, trace_runs=2, no_confirmatory_inference=True,
                  cpu_scopes=SCOPES, per_run=rows, per_block=paired, paired_descriptive=comparisons, absolute=absolute,
                  traces=trace_data, phase_count_comparison=phase, whole_process_count_comparison=whole,
                  provenance=dict(input_root=str(root), execution_manifest_sha256=manifest_sha, registration_sha256=digest(registration),
                                  analyzer_sha256=digest(Path(__file__)), trace_parser_sha256=TRACE_PARSER, frozen_v7_sha256=FROZEN,
                                  input_sha256=pins, raw_files=hashes, R17_observations_used=0))
    dump(out / 'analysis.json', report)
    dump(out / 'provenance.json', report['provenance'])
    for name, data in [('timing-per-run.csv', rows), ('timing-per-block.csv', paired), ('timing-descriptive.csv', comparisons),
                       ('timing-absolute.csv', absolute), ('trace-phase-entry-counts.csv', phase), ('trace-wholeprocess-counts.csv', whole),
                       ('input-files.csv', [dict(path=path, **record) for path, record in hashes.items()])]:
        csv_out(out / name, data)
    for arm in ('go8', 'go2'):
        trace_outputs(out, arm, trace_data[arm], timelines[arm])
    (out / 'TABLES.md').write_text(tables(comparisons, absolute, trace_data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('input_root')
    parser.add_argument('new_output_dir')
    parser.add_argument('--v7-scripts', default=str(Path(__file__).parent / 'analysis-support' / 'frozen-v7-scripts'))
    args = parser.parse_args()
    out = None
    try:
        root = Path(args.input_root).resolve(strict=True)
        target = Path(args.new_output_dir).absolute()
        need(root.is_dir() and not target.exists() and not target.is_symlink() and not target.resolve().is_relative_to(root), 'input must exist; output must be new and outside input')
        complete = read(root / 'complete.json')
        need(complete.get('completed_timing_runs') == 18 and complete.get('completed_trace_runs') == 2, 'R18 not complete; no interim analysis')
        target.mkdir(parents=True, exist_ok=False)
        out = target
        dump(out / 'status.json', dict(status='validating'))
        runner, analyzer, trace_parser = load_helpers(Path(args.v7_scripts).resolve(strict=True))
        analyze(root, out, runner, analyzer, trace_parser)
        dump(out / 'status.json', dict(status='complete', valid_timing_runs=18, valid_trace_runs=2, confirmatory_claim=False))
        print(f'R18 descriptive analysis complete: {out}')
        return 0
    except Exception as error:
        if out is not None:
            dump(out / 'status.json', dict(status='failed', error=str(error), error_type=type(error).__name__, performance_claim_permitted=False))
        print(f'R18 FAILED: {type(error).__name__}: {error}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
