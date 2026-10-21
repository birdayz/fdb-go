#!/usr/bin/env python3
"""Shared offline helpers; importing this module does not contact FDB."""
import datetime
import hashlib
import json
import os
from pathlib import Path

PERF = Path(os.environ['FDB_PERF_HOME']).resolve()
ROOT = PERF / 'rfc-264-latency'
PLAN = Path(__file__).resolve().with_name('plan.json')


def path(value):
    expanded = os.path.expandvars(value)
    if '$' in expanded or not Path(expanded).is_absolute():
        raise ValueError('set every plan path variable to an absolute path: ' + value)
    return Path(expanded)


def sha(file):
    with open(file, 'rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def dump(file, value):
    Path(file).write_text(json.dumps(value, indent=2) + '\n')


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def load():
    return json.loads(PLAN.read_text())


def rotate(xs, n):
    n %= len(xs)
    return xs[n:] + xs[:n]


def schedule(p, slot):
    s = p['slots'][slot]
    arms = [a for a in p['arms'] if a['id'] in s['arms']]
    jobs = []
    for block in range(s['blocks']):
        for cell_position, cell in enumerate(rotate(s['cells'], block)):
            if len(arms) == 2:
                order = rotate(arms, block)
            else:
                order = rotate(arms, 4 * block + 2 * cell_position)
                if block % 2:
                    order = list(reversed(order))
            for position, arm in enumerate(order):
                workload, workers = cell
                counts = p['counts'][str(workers)]
                run_id = f'b{block:02d}-{workload}-w{workers:02d}-p{position:02d}-{arm["id"]}'
                # Equal byte length across slots, cells and arms; only this range is owned.
                prefix = p['prefix_root'] + f's{slot[-1]}/b{block:02d}/c{cell_position:02d}/a{p["arms"].index(arm):02d}/'
                prefix = prefix[:-1].ljust(p['prefix_bytes'] - 1, '_') + '/'
                assert len(prefix.encode()) == p['prefix_bytes']
                jobs.append({'id': run_id, 'run_id': run_id, 'block': block, 'cell_position': cell_position,
                             'position': position, 'arm': arm['id'], 'prefix': prefix,
                             'source_revision': arm['revision'],
                             'workload': dict(name=workload, workers=workers, **counts)})
    assert len(jobs) == s['processes']
    assert len({j['prefix'] for j in jobs}) == len(jobs)
    for arm in arms:
        for workload, workers in s['cells']:
            population = [j['block'] for j in jobs if (j['arm'], j['workload']['name'], j['workload']['workers']) ==
                          (arm['id'], workload, workers)]
            assert sorted(population) == list(range(s['blocks']))
    return jobs
