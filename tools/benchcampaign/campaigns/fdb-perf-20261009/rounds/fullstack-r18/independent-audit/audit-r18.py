#!/usr/bin/env python3
"""Independent local R18 raw/math/trace audit. No production parser imports."""
import csv
from decimal import Decimal
import hashlib
import itertools
import json
import math
from pathlib import Path
import random
import re

D = Path('${FDB_PERF_HOME}/results/fdb-perf-20261009')
ROOT = D / 'fullstack-r18-extracted/fullstack-r18/artifacts/fullstack-r18'
PUBLISHED = ROOT.parent / 'fullstack-r18-analysis'
OUT = Path(__file__).resolve().parent
ARCHIVE_SHA = '2a3b3b79f88008d45cb8c06321525405c437caa13d42d7a6995e20904c6c3842'
ARMS = ('go8', 'go2', 'cgo')
PAIRS = (('go2', 'go8'), ('go8', 'cgo'), ('go2', 'cgo'))
METRICS = ['mean_ns', 'p99_ns'] + [f'{scope}{component}_cpu_ns_per_op'
           for scope in ('window', 'drained', 'shutdown') for component in ('', '_user', '_system')]
SYSCALLS = ('read', 'write', 'readv', 'writev', 'pread64', 'pwrite64', 'sendmsg', 'recvfrom', 'sendto', 'recvmsg',
            'recvmmsg', 'sendmmsg', 'epoll_pwait', 'epoll_wait', 'epoll_ctl', 'futex', 'nanosleep', 'clock_nanosleep', 'sched_yield', 'getrusage')


def need(condition, message):
    if not condition:
        raise ValueError(message)


def read(path):
    return json.loads(path.read_text())


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def dump(name, obj):
    (OUT / name).write_text(json.dumps(obj, indent=2, sort_keys=True, allow_nan=False) + '\n')


def csv_out(name, rows):
    with (OUT / name).open('w', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
        writer.writeheader()
        for row in rows:
            writer.writerow({key: json.dumps(value) if isinstance(value, (list, dict)) else value for key, value in row.items()})


def samples(path, n, phase):
    values = []
    with path.open(newline='') as stream:
        for index, row in enumerate(csv.DictReader(stream)):
            need(row['phase'] == phase and row['index'] == str(index) and row['worker'] == '0', 'raw sample identity')
            need(row['attempts'] == '1' and row['error'] == '' and row['check_status'] == 'ok', 'retry/error/byte check')
            value = int(row['latency_ns'])
            need(value > 0, 'nonpositive latency')
            values.append(value)
    need(len(values) == n, 'sample count differs')
    return values


def snapshot_sequence(summary):
    lifecycle = summary['lifecycle']
    sequence = [summary['stats_after_identity'], summary['stats_before'], summary['stats_after'],
                lifecycle['stats_after_measurement_drain'], lifecycle['stats_after_shutdown']]
    for snapshot in sequence:
        need(all(type(value) is int and value >= 0 for value in snapshot.values()), 'noninteger A-E counter')
    need(all(after[key] >= before[key] for before, after in zip(sequence, sequence[1:]) for key in before), 'decreasing A-E counter')
    need(lifecycle['complete'] and lifecycle['database_closed'] and lifecycle['measurement_barrier_complete'], 'lifecycle incomplete')
    return sequence


def audit_timing(published):
    schedule = read(ROOT / 'global-schedule.json')
    rng = random.Random(798001)
    orders = list(itertools.permutations(ARMS))
    rng.shuffle(orders)
    expected = []
    for block in range(6):
        workload = ['read1000']
        rng.shuffle(workload)
        expected.extend((block, position, arm) for position, arm in enumerate(orders[block]))
    need([(entry['block'], entry['position'], entry['arm']) for entry in schedule] == expected, 'global randomization mismatch')
    rows, hashes = [], []
    pub_rows = {row['run_id']: row for row in published['per_run']}
    for index, entry in enumerate(schedule):
        folder = ROOT / f'run{index:03d}' / entry['run_id']
        summary, manifest, result = [read(folder / name) for name in ('summary.json', 'manifest.json', 'result.json')]
        need(summary['success'] and summary['persisted_validation'] == 'ok' and result['returncode'] == 0, 'failed run')
        need(summary['completed'] == summary['requested'] == 5000 and summary['warmup_completed'] == summary['warmup_requested'] == 200, 'transaction counts')
        need(all(type(summary[key]) is int and summary[key] == 0 for key in ('errors', 'warmup_errors', 'retries', 'warmup_retries')), 'retry/error summary')
        need(summary['config'] == manifest['config'] and summary['backend'] == manifest['backend'], 'identity mismatch')
        need(manifest['gomaxprocs'] == (2 if entry['arm'] == 'go2' else 8), 'GOMAXPROCS mismatch')
        expected_binary = ('4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c' if entry['arm'] == 'cgo'
                           else '93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f')
        need(manifest['backend']['executable_sha256'] == expected_binary, 'binary mismatch')
        latency = samples(folder / 'samples.csv', 5000, 'measure')
        samples(folder / 'warmup.csv', 200, 'warmup')
        A, B, C, D_, E = snapshot_sequence(summary)
        row = dict(run_id=str(folder.relative_to(ROOT)), block=entry['block'], arm=entry['arm'],
                   mean_ns=sum(latency) / 5000, p99_ns=sorted(latency)[4949])
        for scope, before, after in [('window', B, C), ('drained', B, D_), ('shutdown', A, E)]:
            user = (after['process_user_cpu_us'] - before['process_user_cpu_us']) * 1000
            system = (after['process_system_cpu_us'] - before['process_system_cpu_us']) * 1000
            row.update({scope + '_cpu_ns_per_op': (user + system) / 5000,
                        scope + '_user_cpu_ns_per_op': user / 5000, scope + '_system_cpu_ns_per_op': system / 5000})
        for metric in METRICS:
            need(row[metric] == pub_rows[row['run_id']][metric], f'raw statistic mismatch {row["run_id"]} {metric}')
        rows.append(row)
        for name in ('summary.json', 'manifest.json', 'result.json', 'samples.csv', 'warmup.csv'):
            hashes.append(dict(path=str((folder / name).relative_to(ROOT)), sha256=sha(folder / name)))
    need(len(rows) == 18 and len(pub_rows) == 18, 'timing census mismatch')
    by = {(row['block'], row['arm']): row for row in rows}
    need(len(by) == 18, 'duplicate block/arm')
    pub_pairs = {(row['block'], row['numerator'], row['denominator'], row['metric']): row for row in published['per_block']}
    pub_comparisons = {(row['numerator'], row['denominator'], row['metric']): row for row in published['paired_descriptive']}
    pairs, comparisons = [], []
    for numerator, denominator in PAIRS:
        for metric in METRICS:
            ratios = []
            for block in range(6):
                n, d = by[block, numerator][metric], by[block, denominator][metric]
                need(n > 0 and d > 0, 'zero metric requires explicit undefined log ratio handling')
                ratio = n / d
                ratios.append(ratio)
                expected = pub_pairs[block, numerator, denominator, metric]
                need(expected['numerator_value'] == n and expected['denominator_value'] == d and expected['ratio'] == ratio, 'paired value/ratio mismatch')
                pairs.append(dict(block=block, numerator=numerator, denominator=denominator, metric=metric,
                                  numerator_value=n, denominator_value=d, ratio=ratio))
            point = math.exp(math.fsum(math.log(value) for value in ratios) / 6)
            expected = pub_comparisons[numerator, denominator, metric]
            need(expected['geometric_mean_ratio'] == point and expected['ratios_in_block_order'] == ratios
                 and expected['minimum_ratio'] == min(ratios) and expected['maximum_ratio'] == max(ratios), 'descriptive summary mismatch')
            comparisons.append(dict(numerator=numerator, denominator=denominator, metric=metric,
                                    geometric_mean_ratio=point, minimum_ratio=min(ratios), maximum_ratio=max(ratios), ratios=ratios))
    return rows, pairs, comparisons, hashes


def raw_trace_events(folder):
    events, hashes, continuations, unknown_ends = [], [], 0, 0
    for path in sorted(folder.glob('trace.*')):
        need(re.fullmatch(r'trace\.[1-9]\d*', path.name) is not None, 'unexpected trace filename')
        tid = int(path.name.split('.')[1])
        pending = None
        terminal = False
        for line_no, line in enumerate(path.read_text().splitlines(), 1):
            match = re.fullmatch(r'(\d+\.\d+)\s+(.*)', line)
            need(match is not None, f'{path}:{line_no}: unexpected trace format')
            timestamp, body = Decimal(match[1]), match[2]
            if body.startswith('--- SIG'):
                continue
            if body.startswith('+++ ') and body.endswith(' +++'):
                terminal = True
                continue
            resumed = re.match(r'<\.\.\. ([a-z][a-z0-9_]*) resumed>(.*)', body)
            if resumed:
                need(pending is not None and pending['syscall'] == resumed[1], 'unmatched resumed syscall')
                pending['body'] = pending['body'].removesuffix('<unfinished ...>') + resumed[2]
                pending['lines'].append(line_no)
                continuations += 1
                if pending['body'].endswith(' = ?'):
                    unknown_ends += 1
                pending = None
                continue
            entry = re.match(r'([a-z][a-z0-9_]*)\(', body)
            need(entry is not None and entry[1] in SYSCALLS and pending is None, 'invalid/new while pending syscall')
            event = dict(tid=tid, syscall=entry[1], entry=timestamp, body=body, file=path.name, lines=[line_no])
            events.append(event)
            if body.endswith('<unfinished ...>'):
                pending = event
            if body.endswith(' = ?'):
                unknown_ends += 1
        need(terminal, f'{path}: missing terminal record')
        if pending is not None:
            unknown_ends += 1
        hashes.append(dict(path=str(path.relative_to(ROOT)), sha256=sha(path)))
    need(events, 'empty trace population')
    return events, hashes, continuations, unknown_ends


def audit_trace(arm, published):
    folder = ROOT / 'strace-sequence' / arm
    summary = read(folder / 'summary.json')
    manifest = read(folder / 'manifest.json')
    need(summary['completed'] == 200 and summary['warmup_completed'] == 200, 'short trace count')
    samples(folder / 'samples.csv', 200, 'measure')
    samples(folder / 'warmup.csv', 200, 'warmup')
    snapshots = snapshot_sequence(summary)
    events, hashes, continuations, unknown_ends = raw_trace_events(folder)
    need(manifest['pid'] in {event['tid'] for event in events}, 'main PID trace missing')
    markers = {}
    previous = None
    for label, stats in zip('ABCDE', snapshots):
        signature = (stats['process_user_cpu_us'], stats['process_system_cpu_us'], stats['process_high_water_rss_kb'])
        candidates = []
        for event in events:
            if event['syscall'] != 'getrusage' or not event['body'].startswith('getrusage(RUSAGE_SELF,'):
                continue
            u = re.search(r'ru_utime=\{tv_sec=(\d+),\s*tv_usec=(\d+)\}', event['body'])
            s = re.search(r'ru_stime=\{tv_sec=(\d+),\s*tv_usec=(\d+)\}', event['body'])
            rss = re.search(r'ru_maxrss=(\d+)', event['body'])
            need(u and s and rss, 'malformed getrusage tuple')
            actual = (int(u[1])*1_000_000+int(u[2]), int(s[1])*1_000_000+int(s[2]), int(rss[1]))
            if actual == signature:
                elapsed = re.search(r'\) = 0 <(\d+\.\d+)>$', event['body'])
                need(elapsed is not None, 'getrusage not successfully completed with elapsed')
                candidates.append((event, event['entry'] + Decimal(elapsed[1])))
        need(len(candidates) == 1, f'{arm}: marker{label} nonunique/missing')
        event, end = candidates[0]
        need(previous is None or previous <= event['entry'], 'A-E chronology mismatch')
        previous = end
        markers[label] = dict(entry=str(event['entry']), end=str(end), file=event['file'], lines=event['lines'], tid=event['tid'], signature=list(signature))
        expected = published['traces'][arm]['markers'][label]
        need(Decimal(expected['entry_epoch']) == event['entry'] and Decimal(expected['end_epoch']) == end
             and expected['source_file'] == event['file'] and expected['source_lines'] == event['lines'], 'published marker mismatch')
    lower, upper = Decimal(markers['B']['end']), Decimal(markers['C']['entry'])
    need(lower < upper, 'invalid phase window')
    tids = sorted(int(path.name.split('.')[1]) for path in folder.glob('trace.*'))
    whole = {tid: dict.fromkeys(SYSCALLS, 0) for tid in tids}
    phase = {tid: dict.fromkeys(SYSCALLS, 0) for tid in tids}
    for event in events:
        whole[event['tid']][event['syscall']] += 1
        if lower <= event['entry'] < upper:
            phase[event['tid']][event['syscall']] += 1
    for population, counts in [('whole_short_process', whole), ('measured_window', phase)]:
        expected = published['traces'][arm][population]
        need(sum(sum(row.values()) for row in counts.values()) == expected['total_entries'], f'{arm}: total entries mismatch')
        for syscall in SYSCALLS:
            need(sum(row[syscall] for row in counts.values()) == expected['aggregate'][syscall]['calls'], f'{arm}: aggregate {population} {syscall}')
            for tid, row in counts.items():
                need(row[syscall] == expected['per_tid'][str(tid)][syscall]['calls'], f'{arm}: per-TID {population} {syscall}')
    return dict(arm=arm, markers=markers, measured_window=dict(lower_inclusive=str(lower), upper_exclusive=str(upper)),
                whole_entries=sum(sum(row.values()) for row in whole.values()), phase_entries=sum(sum(row.values()) for row in phase.values()),
                whole_per_tid=whole, phase_per_tid=phase, unfinished_resumed_pairs=continuations,
                unknown_terminal_ends=unknown_ends), hashes


def main():
    try:
        need(sha(D / 'fullstack-r18.tar.gz') == ARCHIVE_SHA, 'archive digest mismatch')
        published = read(PUBLISHED / 'analysis.json')
        rows, pairs, comparisons, hashes = audit_timing(published)
        traces = {}
        for arm in ('go8', 'go2'):
            traces[arm], more = audit_trace(arm, published)
            hashes.extend(more)
        counts = []
        for syscall in SYSCALLS:
            c8 = sum(row[syscall] for row in traces['go8']['phase_per_tid'].values())
            c2 = sum(row[syscall] for row in traces['go2']['phase_per_tid'].values())
            counts.append(dict(syscall=syscall, go8_phase_entries=c8, go2_phase_entries=c2,
                               go8_phase_entries_divided_by_200=c8/200, go2_phase_entries_divided_by_200=c2/200))
        output = dict(status='passed', independent_of_production_code=True, production_parser_imported=False,
                      sealed_archive_sha256=ARCHIVE_SHA, sealed_files_modified=False, timing_runs=18, measured_timing_samples=90000,
                      warmup_timing_samples=3600, paired_blocks=6, individual_timing_ratios=len(pairs), descriptive_comparisons=len(comparisons),
                      timing_values_ratios_GMs_and_ranges_exactly_match=True, all_trace_aggregate_and_per_tid_whole_phase_counts_exactly_match=True,
                      comparisons=comparisons, trace_audit=traces, phase_counts=counts,
                      inference_scope='Descriptive six-block ratios/GM/observed ranges only. No confidence intervals, significance, equivalence, shipping recommendation or R17 rescue. Trace counts are a separate instrumented population, not CPU/latency evidence; no exact per-operation alignment.',
                      allocation_scope='No allocation or native-heap comparisons audited or claimed.',
                      audit_scope='Does not repeat every host gate or all2132 archive-member checks. Whole archive digest independently verified.',
                      code_sha256=sha(Path(__file__)))
        dump('audit.json', output)
        csv_out('recomputed-timing-per-run.csv', rows)
        csv_out('recomputed-timing-per-block.csv', pairs)
        csv_out('recomputed-timing-descriptive.csv', comparisons)
        csv_out('recounted-trace-phase.csv', counts)
        csv_out('raw-files-read.csv', hashes)
        text = ['# Independent R18 raw/math/trace audit\n',
                'PASS:18 raw timing runs;198 individual paired ratios and33 descriptive GM/range summaries across three comparisons exactly match published values. '
                'Both traces independently recounted from original trace.<tid> files with Decimal timestamps and exact A–E user/system/RSS matches. '
                'All whole-process and measured-window per-TID/aggregate syscall entry counts match.\n',
                '|go2/go8 metric|Paired GM|Observed six-ratio range|Six ratios in block order|\n|---|---:|---|---|\n']
        for item in comparisons:
            if item['numerator'] == 'go2' and item['denominator'] == 'go8' and item['metric'] in ('mean_ns','p99_ns','window_cpu_ns_per_op','drained_cpu_ns_per_op','shutdown_cpu_ns_per_op'):
                text.append(f"|{item['metric']}|{item['geometric_mean_ratio']:.9f}|[{item['minimum_ratio']:.6f},{item['maximum_ratio']:.6f}]|"+
                            ', '.join(f'{value:.6f}' for value in item['ratios'])+'|\n')
        text.append('\n|Trace syscall|go8 phase entries|go2 phase entries|go8 /200|go2 /200|\n|---|---:|---:|---:|---:|\n')
        for item in counts:
            if item['syscall'] in ('futex','epoll_pwait','epoll_wait','epoll_ctl','nanosleep','clock_nanosleep'):
                text.append(f"|{item['syscall']}|{item['go8_phase_entries']}|{item['go2_phase_entries']}|{item['go8_phase_entries_divided_by_200']:.6f}|{item['go2_phase_entries_divided_by_200']:.6f}|\n")
        text.append('\nNo CI or effect-significance conclusion is calculated. These are observed descriptive ratios, not a shipping recommendation. '
                    'Trace scheduling is perturbed and cannot establish CPU/latency causality;200-trace transactions are not pooled with5000-timing transactions. '
                    'No native-heap/allocation claim. No sealed file modified; no VM/network actions.\n')
        (OUT / 'AUDIT.md').write_text('\n'.join(text))
        print('\n'.join(text))
    except Exception as error:
        dump('audit.json', dict(status='failed', error=str(error), error_type=type(error).__name__, sealed_files_modified=False))
        raise


if __name__ == '__main__':
    main()
