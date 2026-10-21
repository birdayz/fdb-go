#!/usr/bin/env python3
"""Validate syntax, pins, schedules and estimators without builds or FDB access."""
import ast
from collections import Counter
import csv
import json
import math
import tempfile
from pathlib import Path
from unittest.mock import patch

from common import ROOT, PLAN, dump, load, path, schedule, sha
import timing


def check():
    p = load()
    syntax = []
    for source in sorted(Path(__file__).resolve().parent.glob('*.py')):
        ast.parse(source.read_text(), filename=str(source))
        syntax.append(source.name)
    assert len(p['arms']) == 12
    assert sum(a.get('build', False) for a in p['arms']) == 10
    assert len({a['revision'] for a in p['arms']}) == 12
    assert all(len(a['revision']) == 40 for a in p['arms'])
    assert sha(path(p['harness_archive'])) == p['harness_id']
    assert sha(Path(__file__).resolve().with_name('frozen_run.py')) == p['validator_sha256']
    assert sha(path(p['cluster_file'])) == p['cluster_file_sha256']
    assert sha(path(p['fdbcli'])) == p['fdbcli_sha256']
    assert len(timing.binaries(p, 'slot1')) == 2
    ledger = list(csv.DictReader(Path(__file__).resolve().with_name('client-commits.tsv').open(), delimiter='\t'))
    assert len(ledger) == 19
    assert ledger[0]['commit'] == p['arms'][1]['revision']
    assert ledger[-1]['commit'] == p['arms'][-1]['revision']
    jobs = {slot: schedule(p, slot) for slot in ('slot1', 'slot2')}
    assert len(jobs['slot1']) == 32 and len(jobs['slot2']) == 216
    prefixes = [j['prefix'] for group in jobs.values() for j in group]
    assert len(prefixes) == len(set(prefixes)) == 248
    assert {len(v.encode()) for v in prefixes} == {68}
    assert p['prefix_end'] == p['prefix_root'][:-1] + '0'
    for workload, workers in p['slots']['slot1']['cells']:
        cell = [j for j in jobs['slot1'] if (j['workload']['name'], j['workload']['workers']) == (workload, workers)]
        orders = Counter(tuple(j['arm'] for j in cell if j['block'] == b) for b in range(4))
        assert orders == {('old', 'master'): 2, ('master', 'old'): 2}
    for workload, workers in p['slots']['slot2']['cells']:
        cell = [j for j in jobs['slot2'] if (j['workload']['name'], j['workload']['workers']) == (workload, workers)]
        for arm in p['slots']['slot2']['arms']:
            mine = [j for j in cell if j['arm'] == arm]
            assert {j['block'] for j in mine} == {0, 1, 2}
            assert len({j['position'] for j in mine}) == 3
    # Exercise the actual raw-quantile function with generated samples, not FDB.
    class NoOpValidator:
        def validate_raw_output(self, *args):
            pass
        def validate_identity(self, *args):
            pass
    with tempfile.TemporaryDirectory(prefix='latency-offline-') as tmp:
        folder = Path(tmp)
        with (folder / 'samples.csv').open('w', newline='') as f:
            writer = csv.DictWriter(f, fieldnames=['latency_ns', 'attempts'])
            writer.writeheader()
            writer.writerows({'latency_ns': n, 'attempts': 1} for n in range(1, 101))
        for name in ('warmup.csv', 'summary.json', 'manifest.json'):
            (folder / name).write_text('{}\n')
        synthetic = timing.validate(p, jobs['slot1'][0], {'old': {'binary': '/synthetic/old', 'sha256': '0'*64, 'revision': p['arms'][0]['revision']}},
                                    folder, NoOpValidator())
        assert synthetic['p50_ns'] == 50 and synthetic['p99_ns'] == 99
        assert synthetic['operations'] == 100 and synthetic['retries'] == 0
        fake_proc = folder / 'proc'
        fake_proc.mkdir()
        masks = {100: {8, 20}, 101: {9, 21}, 102: {10, 22}}
        status = {'cluster': {'processes': {}}}
        for pid, port in zip(masks, (4500, 4501, 4502)):
            process = fake_proc / str(pid)
            process.mkdir()
            (process / 'cmdline').write_bytes(b'\0'.join([b'/fake/fdbserver', b'-p', f'127.0.0.1:{port}'.encode(), b'']))
            status['cluster']['processes'][str(pid)] = {'address': f'127.0.0.1:{port}'}
        route = lambda value: fake_proc if value == '/proc' else Path(value)
        with patch.object(timing, 'Path', side_effect=route), patch.object(timing.os, 'sched_getaffinity', side_effect=lambda pid: masks[pid]):
            assert len(timing.server_affinities(status)) == 3
            masks[100] = {8, 9, 10, 20, 21, 22}
            try:
                timing.server_affinities(status)
            except RuntimeError:
                pass
            else:
                raise AssertionError('union mask incorrectly accepted for a single server')
    values = [1.1, 1.2, 1.3, 1.4]
    result = timing.stats(values, 4)
    assert math.isclose(result['geometric_mean'], math.prod(values) ** 0.25)
    assert result['block_min'] == 1.1 and result['block_max'] == 1.4
    assert result['range_label'] == 'block min/max, not CI'
    try:
        timing.stats([], 0)
    except RuntimeError:
        pass
    else:
        raise AssertionError('empty ratio population accepted')
    # Import the sealed original validator; no runner or subprocess is invoked.
    frozen = timing.validator(p)
    assert callable(frozen.validate_raw_output) and callable(frozen.validate_identity)
    report = {'valid': True, 'scope': 'offline syntax/pins/schedules/synthetic estimators only; no builds, benchmarks, cluster calls or test lane',
              'syntax_files': syntax, 'client_commit_census': len(ledger), 'interiors': 10,
              'processes': {s: len(js) for s, js in jobs.items()}, 'unique_equal_length_prefixes': len(prefixes),
              'slot1_arm_orders': 'each cell: two old/master and two master/old',
              'slot2_rotations': 'each arm/cell has all three blocks and three distinct positions',
              'synthetic_quantiles': {'p50': synthetic['p50_ns'], 'p99': synthetic['p99_ns']},
              'plan_sha256': sha(PLAN)}
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    check()
