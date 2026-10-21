"""Bound CSV/trace alignment; instrumented mechanism evidence, not latency pricing.

Consume rev5-trace-extract.py output. No guessed absolute epoch or pooling.
Raw per-wait/per-slow-sample tables and JSON summaries are written under evidence.
"""
import argparse
import bisect
import csv
import hashlib
import json
import pathlib
import statistics


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def describe(values):
    values = sorted(values)
    if not values:
        return {'count': 0, 'sum_ns': 0}
    return {'count': len(values), 'sum_ns': sum(values), 'min_ns': values[0],
            'median_ns': statistics.median(values), 'p99_ns': values[int((len(values)-1)*.99)],
            'max_ns': values[-1]}


def analyze(root, arm, margin):
    out = root / 'trace-analysis'
    events = out / (arm + '.events.jsonl')
    records = [json.loads(line) for line in events.open()]
    end = records[-1]
    assert end['type'] == 'end' and end['unfinished_weak_waits'] == 0
    workers = [r['data'] for r in records if r['type'] == 'worker']
    waits = [r for r in records if r['type'] == 'weak_wait']
    run = root / 'runs' / ('tail-diag-trace-range100-p0-' + arm)
    manifest = json.loads((run / 'manifest.json').read_text())
    summary = json.loads((run / 'summary.json').read_text())
    assert manifest['go_version'] == 'go1.26.9'
    assert manifest['config']['profile'] == 'trace' and manifest['config'].get('rate', 0) == 0
    assert len(workers) == manifest['config']['workers'] == 32
    frozen_runs = json.loads((out / 'extraction-inputs.json').read_text())['runs']
    if arm not in frozen_runs:
        frozen_runs.update(json.loads((out / ('extraction-inputs.' + arm + '.json')).read_text())['runs'])
    frozen = frozen_runs[arm]
    input_hashes = {'compact_events_sha256': digest(events),
                    'samples_sha256': digest(run / 'samples.csv'),
                    'trace_sha256': digest(run / 'trace.out')}
    assert input_hashes['compact_events_sha256'] == frozen['events_sha256']
    assert input_hashes['samples_sha256'] == frozen['samples_sha256']
    assert input_hashes['trace_sha256'] == frozen['trace_sha256']
    assert digest(pathlib.Path(manifest['backend']['executable'])) == manifest['backend']['executable_sha256']
    rows = []
    by_worker = [[] for _ in workers]
    for row in csv.DictReader((run / 'samples.csv').open()):
        assert row['phase'] == 'measure' and not row['error'] and row['check_status'] == 'ok'
        sample = dict(index=int(row['index']), worker=int(row['worker']),
                      start=int(row['start_offset_ns']), latency=int(row['latency_ns']))
        sample['end'] = sample['start'] + sample['latency']
        rows.append(sample)
        by_worker[sample['worker']].append(sample)
    assert len(rows) == manifest['config']['iterations'] == summary['completed']
    for worker, samples in enumerate(by_worker):
        assert all(s['worker'] == worker and s['index'] % len(workers) == worker for s in samples)
        assert all(a['end'] <= b['start'] for a, b in zip(samples, samples[1:]))
    # All workers must have actually blocked on runPhase's initial <-start.
    assert all(w['created'] < w['first_running'] <= w['start_wait'] < w['release'] <= w['after_release_running'] for w in workers)
    lower = max(w['after_release_running'] - by_worker[w['worker']][0]['start'] for w in workers) - margin
    upper = min(w['release'] for w in workers) + margin
    assert lower <= upper, (lower, upper)
    clocks = [r['mono'] - r['trace'] for r in records if r['type'] == 'sync']
    assert clocks and max(map(abs, clocks)) <= margin
    ranges = [r for r in records if r['type'] == 'range' and r['name'] == 'GC concurrent mark phase']
    marks = []
    active = None
    active_checkpoints = 0
    for r in ranges:
        if r['kind'] == 'RangeActive':
            # Generation-boundary carry-forward, not a second mark interval.
            # Still reject genuinely partial initial intervals rather than guess.
            assert active is not None and active <= r['time']
            active_checkpoints += 1
            continue
        if r['kind'] == 'RangeBegin':
            assert active is None
            active = r['time']
        else:
            assert active is not None and active <= r['time']
            marks.append((active, r['time']))
            active = None
    assert active is None
    starts = [g[0] for g in marks]
    ends = [g[1] for g in marks]
    assert all(a[1] < b[0] for a, b in zip(marks, marks[1:]))

    def overlaps(sample):
        # Half-open intervals. These are existential / universal bounds over
        # the SAME unknown epoch, not a point-estimated alignment.
        left = bisect.bisect_right(ends, lower + sample['start'] - margin)
        right = bisect.bisect_left(starts, upper + sample['end'] + margin)
        possible = list(range(left, right))
        definite = [i for i in possible if upper + sample['start'] < marks[i][1] - margin
                    and lower + sample['end'] > marks[i][0] + margin]
        return definite, possible

    sample_starts = [[s['start'] for s in ss] for ss in by_worker]
    sample_ends = [[s['end'] for s in ss] for ss in by_worker]
    definite_waits, possible_waits = {}, {}
    for i, wait in enumerate(waits):
        wait['wait_id'] = i
        assert wait['begin'] < wait['unpark'] <= wait['running']
        wait['park_ns'] = wait['unpark'] - wait['begin']
        wait['runnable_ns'] = wait['running'] - wait['unpark']
        wait['arm_timeout_stack'] = 'armTimeoutLocked' in wait['stack']
        wait['gc_mark_at_begin'] = bisect.bisect_right(starts, wait['begin']) - 1
        mi = wait['gc_mark_at_begin']
        wait['begin_in_gc_mark'] = mi >= 0 and wait['begin'] < marks[mi][1]
        wait['possible_samples'] = []
        wait['definite_samples'] = []
        worker = wait['worker']
        if worker < 0:
            continue
        first = bisect.bisect_left(sample_ends[worker], wait['running'] - margin - upper)
        last = bisect.bisect_right(sample_starts[worker], wait['begin'] + margin - lower)
        for sample in by_worker[worker][first:last]:
            # Full park + runnable span must fit the same sample at one epoch.
            lo = max(lower, wait['running'] - margin - sample['end'])
            hi = min(upper, wait['begin'] + margin - sample['start'])
            if lo <= hi:
                index = sample['index']
                wait['possible_samples'].append(index)
                possible_waits.setdefault(index, []).append(i)
                if upper + sample['start'] <= wait['begin'] - margin and lower + sample['end'] >= wait['running'] + margin:
                    wait['definite_samples'].append(index)
                    definite_waits.setdefault(index, []).append(i)
        assert wait['possible_samples'], ('unmapped worker wait', wait)
    arm_waits = [w for w in waits if w['arm_timeout_stack']]
    ranked = sorted(rows, key=lambda x: (-x['latency'], x['index']))
    quantiles = {name: ranked[len(rows)-1-int((len(rows)-1)*p)]['latency']
                 for name, p in [('p50', .5), ('p90', .9), ('p99', .99), ('p999', .999)]}
    quantiles.update(mean=sum(s['latency'] for s in rows)/len(rows),
                     minimum=ranked[-1]['latency'], maximum=ranked[0]['latency'])
    tail = []
    for sample in ranked[:max(1, len(rows)//1000)]:
        definite, possible = overlaps(sample)
        tail.append(dict(sample, absolute_begin_bounds_ns=[lower+sample['start'], upper+sample['start']],
                         absolute_end_bounds_ns=[lower+sample['end'], upper+sample['end']],
                         definite_gc_marks=definite, possible_gc_marks=possible,
                         definite_weak_waits=definite_waits.get(sample['index'], []),
                         possible_weak_waits=possible_waits.get(sample['index'], [])))
    groups = {}
    for name, samples in [('all', rows), ('slowest_1_percent', ranked[:len(rows)//100]),
                          ('slowest_0_1_percent', ranked[:len(rows)//1000])]:
        gc_definite = gc_possible = weak_definite = weak_possible = 0
        for s in samples:
            definite, possible = overlaps(s)
            gc_definite += bool(definite)
            gc_possible += bool(possible)
            weak_definite += s['index'] in definite_waits
            weak_possible += s['index'] in possible_waits
        groups[name] = dict(count=len(samples), definite_gc_overlap=gc_definite,
                            possible_gc_overlap=gc_possible, definite_contains_weak_wait=weak_definite,
                            possible_contains_weak_wait=weak_possible)
    details = out / (arm + '.alignment.json')
    details.write_text(json.dumps({'epoch_bounds_ns': [lower, upper], 'workers': workers,
                                  'gc_mark_intervals_ns': marks, 'weak_waits': waits,
                                  'slowest_0_1_percent': tail}, indent=2) + '\n')
    return dict(arm=arm, events=end['events'], samples=len(rows),
                source_revision=manifest['config']['source_revision'],
                binary_sha256=manifest['backend']['executable_sha256'],
                epoch_bounds_ns=[lower, upper], epoch_width_ns=upper-lower,
                clock_snapshot_mono_minus_trace_ns=clocks, clock_margin_ns=margin,
                gc_mark_intervals=len(marks), gc_active_checkpoints=active_checkpoints,
                instrumented_latency_ns=quantiles,
                total_weak_waits=len(waits),
                arm_timeout_weak_waits=len(arm_waits),
                park=describe([w['park_ns'] for w in arm_waits]),
                runnable=describe([w['runnable_ns'] for w in arm_waits]),
                waits_begin_in_gc_mark=sum(w['begin_in_gc_mark'] for w in arm_waits),
                definite_full_sample_matches=sum(bool(w['definite_samples']) for w in arm_waits),
                possible_full_sample_matches=sum(bool(w['possible_samples']) for w in arm_waits),
                sample_groups=groups, raw_details=str(details), raw_details_sha256=digest(details),
                **input_hashes)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('evidence', type=pathlib.Path)
    p.add_argument('--arms', nargs='+', default=['rev5', 'rev4', 'baseline'])
    p.add_argument('--clock-margin-ns', type=int, default=64,
                   help='one trace clock quantum; checked against every clock snapshot')
    args = p.parse_args()
    result = {'scope': 'instrumented trace mechanism, NOT qualification latency or cause proof',
              'script_sha256': digest(pathlib.Path(__file__)),
              'runs': [analyze(args.evidence, arm, args.clock_margin_ns) for arm in args.arms]}
    output = args.evidence / 'trace-analysis' / 'alignment-summary.json'
    output.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
