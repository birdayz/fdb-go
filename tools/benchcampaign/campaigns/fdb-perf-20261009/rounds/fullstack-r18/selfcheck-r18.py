#!/usr/bin/env python3
"""Artifact-only synthetic R18 statistics, trace-boundary and acquisition-gate checks."""
import ast
import copy
import csv
import importlib.util
import json
import math
from pathlib import Path
import tempfile


def load(path):
    spec = importlib.util.spec_from_file_location('r18_selfcheck_analyzer', path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def rejects(callback, extra=()):
    try:
        callback()
    except (AssertionError, ValueError, KeyError) + extra:
        return
    raise AssertionError('invalid fixture accepted')


def snapshot(index):
    return dict(go_total_alloc_bytes=index * 1000, go_mallocs=index * 10,
                go_gc_cycles=[0, 1, 2, 4, 6][index - 1], go_gc_pause_ns=index * 100,
                process_user_cpu_us=index * 100, process_system_cpu_us=index * 10,
                process_high_water_rss_kb=1000)


def summary(arm, count):
    def counters(gc):
        return dict(finalizers_queued=0, finalizers_executed=0, cleanups_queued_estimate=0,
                    cleanups_executed_estimate=0, gc_cycles=gc)
    def drain(gc):
        return dict(before=counters(gc), after=counters(gc + 2), rounds=2, wall_ns=1, complete=True)
    return dict(success=True, persisted_validation='ok', failure='', requested=count, completed=count, errors=0,
                retries=0, warmup_requested=200, warmup_completed=200, warmup_errors=0, warmup_retries=0,
                measured_wall_ns=100, backend=dict(native_library=arm == 'cgo'),
                stats_after_identity=snapshot(1), stats_before=snapshot(2), stats_after=snapshot(3),
                lifecycle=dict(complete=True, database_closed=True, network_stopped=arm == 'cgo', measurement_barrier_complete=True,
                               measurement_drain=drain(2), shutdown_drain=drain(4),
                               stats_after_measurement_drain=snapshot(4), stats_after_shutdown=snapshot(5)),
                client_counters_delta=dict(values=dict(transaction_retries=0, connection_failures=0, grv_cache_hits=0,
                                                       read_versions_completed=count, grv_successful_batch_replies=count,
                                                       commit_started=0, commit_completed=0)))


def sample_text(count, phase):
    return ('phase,index,worker,start_offset_ns,latency_ns,attempts,error,check_status\n' +
            ''.join(f'{phase},{i},0,{i * 2},1,1,,ok\n' for i in range(count)))


def main():
    work = Path(__file__).resolve().parent
    analyzer = load(work / 'analyze-r18.py')
    runner, frozen, trace = analyzer.load_helpers(work / 'analysis-support' / 'frozen-v7-scripts')
    ratios = [.5, .75, 1., 1.25, 1.5, 2.]
    rows = []
    for block in range(6):
        for arm, multiplier in [('go8', 1), ('go2', ratios[block]), ('cgo', 2)]:
            row = dict(block=block, arm=arm, run_id=f'{block}/{arm}', absolute_E_cpu_ns_per_op=123)
            row.update({metric: (block + 1) * (index + 1) * 100 * multiplier for index, metric in enumerate(analyzer.METRICS)})
            rows.append(row)
    pairs, comparisons, absolute = analyzer.descriptive(rows)
    expected = math.exp(math.fsum(math.log(value) for value in ratios) / 6)
    for item in comparisons:
        if (item['numerator'], item['denominator']) == ('go2', 'go8'):
            assert item['ratios_in_block_order'] == ratios
            assert item['geometric_mean_ratio'] == expected
            assert (item['minimum_ratio'], item['maximum_ratio']) == (.5, 2.)
        if (item['numerator'], item['denominator']) == ('go8', 'cgo'):
            assert item['ratios_in_block_order'] == [.5] * 6
    assert not any(item['metric'] == 'absolute_E_cpu_ns_per_op' for item in pairs)
    assert len(pairs) == 6 * 3 * len(analyzer.METRICS)
    mutated = copy.deepcopy(rows)
    mutated[1]['window_user_cpu_ns_per_op'] = 0
    _, comparisons, _ = analyzer.descriptive(mutated)
    target = next(item for item in comparisons if item['numerator'] == 'go2' and item['denominator'] == 'go8'
                  and item['metric'] == 'window_user_cpu_ns_per_op')
    assert target['geometric_mean_ratio'] is None and len(target['ratios_in_block_order']) == 6
    rejects(lambda: analyzer.descriptive(rows[:-1]))
    rejects(lambda: analyzer.descriptive(rows[:-1] + [rows[0]]))
    rejects(lambda: analyzer.run_statistics(dict(summary=summary('go8', 200), entry={}, latencies=[1] * 200), Path('.'), {}, frozen))
    with tempfile.TemporaryDirectory(prefix='r18-trace-check-', dir=work) as temporary:
        folder = Path(temporary) / 'go8'
        folder.mkdir()
        data = summary('go8', 200)
        config = dict(workload='read1000', iterations=200, warmup=200, workers=1, run_id='synthetic-trace')
        data['config'] = config
        (folder / 'summary.json').write_text(json.dumps(data))
        (folder / 'manifest.json').write_text(json.dumps(dict(config=config, backend=data['backend'], pid=10)))
        (folder / 'result.json').write_text(json.dumps(dict(returncode=0, run_id='synthetic-trace')))
        (folder / 'samples.csv').write_text(sample_text(200, 'measure'))
        def marker(at, index):
            s = snapshot(index)
            return (f'{at:.6f} getrusage(RUSAGE_SELF, {{ru_utime={{tv_sec=0, tv_usec={s["process_user_cpu_us"]}}}, '
                    f'ru_stime={{tv_sec=0, tv_usec={s["process_system_cpu_us"]}}}, ru_maxrss=1000}}) = 0 <0.000001>')
        lines = [marker(1000, 1), marker(1001, 2),
                 '1001.100000 write(7, "y", 1 <unfinished ...>',
                 '1001.150000 <... write resumed>) = 1 <0.050000>',
                 marker(1002, 3), marker(1003, 4), marker(1004, 5), '1005.000000 +++ exited with 0 +++']
        (folder / 'trace.10').write_text('\n'.join(lines) + '\n')
        (folder / 'trace.11').write_text('1000.900000 read(7, <unfinished ...>\n'
            '1001.200000 <... read resumed>"x", 1) = 1 <0.300000>\n'
            '1001.500000 futex(0x123, FUTEX_WAIT_PRIVATE, 0, NULL) = 0 <0.700000>\n'
            '1004.500000 nanosleep({tv_sec=0, tv_nsec=20000} <unfinished ...>) = ?\n'
            '1005.000000 +++ exited with 0 +++\n')
        parsed, timeline = trace.parse_sequence(folder)
        assert parsed['whole_short_process']['total_entries'] == 9
        assert parsed['measured_window']['total_entries'] == 2
        assert parsed['unfinished_resumed_count'] == 2 and parsed['unknown_completion_count'] == 1
        assert parsed['boundary_overlap_counts'] == {'B_end:observed': 1, 'C_start:observed': 1}
        analyzer.strip_whole_normalization(parsed)
        assert 'wholeprocess_calls_divided_by_200' not in json.dumps(parsed)
        assert sum(event['entry_in_measured_window'] for event in timeline) == 2
        # Ambiguous marker signatures must fail closed, not select an arbitrary occurrence.
        (folder / 'trace.10').write_text('\n'.join([lines[0], marker(1000.1, 1), *lines[1:]]) + '\n')
        rejects(lambda: trace.parse_sequence(folder), (trace.FormatError,))
    tree = ast.parse((work / 'acquire-r18.py').read_text())
    nodes = [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in ('read', 'check_observations')]
    assert len(nodes) == 2
    namespace = dict(json=json, csv=csv, runner=runner)
    exec(compile(ast.Module(body=nodes, type_ignores=[]), 'acquire-r18.py:two-validation-functions', 'exec'), namespace)
    check = namespace['check_observations']
    rejected = 0
    with tempfile.TemporaryDirectory(prefix='r18-acquisition-check-', dir=work) as temporary:
        for arm, count in [('go8', 5000), ('go2', 5000), ('cgo', 5000), ('go8', 200), ('go2', 200)]:
            folder = Path(temporary) / f'{arm}-{count}'
            folder.mkdir()
            entry = dict(arm=arm, workload=dict(name='read1000', iterations=count, warmup=200, workers=1))
            base = summary(arm, count)
            (folder / 'summary.json').write_text(json.dumps(base))
            (folder / 'samples.csv').write_text(sample_text(count, 'measure'))
            (folder / 'warmup.csv').write_text(sample_text(200, 'warmup'))
            check(folder, entry)
            for key in ('errors', 'warmup_errors', 'retries', 'warmup_retries'):
                for bad in (1, False):
                    mutated = copy.deepcopy(base); mutated[key] = bad
                    (folder / 'summary.json').write_text(json.dumps(mutated))
                    rejects(lambda: check(folder, entry)); rejected += 1
            for key, bad in [('failure', 'bad'), ('warmup_requested', 201)]:
                mutated = copy.deepcopy(base); mutated[key] = bad
                (folder / 'summary.json').write_text(json.dumps(mutated))
                rejects(lambda: check(folder, entry)); rejected += 1
            (folder / 'summary.json').write_text(json.dumps(base))
            for filename in ('samples.csv', 'warmup.csv'):
                text = (folder / filename).read_text()
                for bad in ('\n'.join(text.splitlines()[:-1]) + '\n', text.replace(',1,1,,ok', ',1,2,,ok', 1)):
                    (folder / filename).write_text(bad)
                    rejects(lambda: check(folder, entry)); rejected += 1
                (folder / filename).write_text(text)
            if arm != 'cgo':
                for key in ('transaction_retries', 'connection_failures', 'grv_cache_hits'):
                    for bad in (1, False):
                        mutated = copy.deepcopy(base); mutated['client_counters_delta']['values'][key] = bad
                        (folder / 'summary.json').write_text(json.dumps(mutated))
                        rejects(lambda: check(folder, entry)); rejected += 1
                for key in ('read_versions_completed', 'grv_successful_batch_replies', 'commit_started', 'commit_completed'):
                    mutated = copy.deepcopy(base); mutated['client_counters_delta']['values'][key] += 1
                    (folder / 'summary.json').write_text(json.dumps(mutated))
                    rejects(lambda: check(folder, entry)); rejected += 1
            (folder / 'summary.json').write_text(json.dumps(base))
            check(folder, entry)
    assert rejected == 110
    print('PASS: six-block GM/ranges/individual ratios, zero-without-exclusion, no absolute-E pairing, short-run/timing separation.')
    print('PASS: exact A-E markers, split unfinished/resumed reconstruction, carry-in/out counts, unknown exit, ambiguous marker rejection.')
    print('PASS: five synthetic timing/trace arm fixtures and 110 acquisition validity mutations; no acquisition code executed.')


if __name__ == '__main__':
    main()
