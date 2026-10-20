#!/usr/bin/env python3
"""Execute the complete frozen R17 schedule once, with no interim inference."""
import csv
import datetime
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys

ROOT = Path('/opt/fdbperf/artifacts/fullstack-r17-timing')
RUNNER = Path('/opt/fdbperf/sources/r17-frozen-harness/tools/clientbench/scripts/run.py')
active = None

def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()

def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()

def read(path):
    return json.loads(path.read_text())

def write(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')

def check_zero_retry_segment(folder, schedule):
    # Counts only: no latency/CPU estimates or interim inference.
    for run in schedule:
        raw = folder/run['run_id']
        summary = read(raw/'summary.json')
        for field in ['errors','warmup_errors','retries','warmup_retries']:
            assert type(summary[field]) is int and summary[field] == 0, (raw, field)
        assert summary['failure'] == '' and summary['warmup_requested'] == 200
        for filename, expected in [('samples.csv',run['workload']['iterations']),('warmup.csv',200)]:
            with (raw/filename).open(newline='') as stream:
                rows = list(csv.DictReader(stream))
            assert len(rows) == expected, (raw, filename)
            assert all(row['attempts'] == '1' for row in rows), (raw, filename, 'callback retries')
        if run['arm'] == 'new':
            counters = summary['client_counters_delta']['values']
            for field in ['transaction_retries','connection_failures','grv_cache_hits']:
                assert type(counters[field]) is int and counters[field] == 0, (raw, field)
            n = run['workload']['iterations']
            assert counters['read_versions_completed'] == counters['grv_successful_batch_replies'] == n
            assert counters['commit_started'] == counters['commit_completed'] == (0 if run['workload']['name'] == 'read1000' else n)

def interrupted(signum, _frame):
    raise KeyboardInterrupt(f'received signal {signum}')

def stop_active():
    global active
    if active is None or active.poll() is not None:
        return
    # The frozen runner handles TERM and drains its own benchmark process group.
    active.terminate()
    try:
        active.wait(timeout=30)
    except subprocess.TimeoutExpired:
        os.killpg(active.pid, signal.SIGKILL)
        active.wait()

manifest_path = ROOT/'execution-manifest.json'
manifest = read(manifest_path)
manifest_sha = digest(manifest_path)
assert manifest['schema'] == 'r17-acquisition-v1'
assert manifest['blocks_per_workload'] == 160 and manifest['scheduled_runs'] == 640
assert len(manifest['segments']) == manifest['scheduled_segments'] == 80
assert not (ROOT/'STARTED.json').exists(), 'Never rerun an attempted confirmatory campaign'
assert not (ROOT/'complete.json').exists() and not (ROOT/'ABORTED.json').exists()
for segment in manifest['segments']:
    assert not (ROOT/segment['name']).exists()
    assert digest(ROOT/segment['plan']) == segment['plan_sha256']
    assert digest(ROOT/segment['schedule']) == segment['schedule_sha256']
    assert read(ROOT/segment['plan'])['expected_sha256'] == manifest['input_sha256']
    assert len(read(ROOT/segment['schedule'])) == 8
for path, expected in manifest['input_sha256'].items():
    assert digest(path) == expected, path
write(ROOT/'STARTED.json', {'started_utc':utc(),'execution_manifest_sha256':manifest_sha})
completed = []
for sig in [signal.SIGTERM, signal.SIGINT]:
    signal.signal(sig, interrupted)
try:
    for segment in manifest['segments']:
        name = segment['name']
        assert digest(manifest_path) == manifest_sha
        print(f'BEGIN {name} {utc()}', flush=True)
        with (ROOT/(name+'-acquisition.log')).open('x') as log:
            active = subprocess.Popen(['taskset','-c','0-1','python3','-B',str(RUNNER),
                '--plan',str(ROOT/segment['plan']),'--output',str(ROOT/name)],
                stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            rc = active.wait()
        active = None
        if rc != 0:
            raise RuntimeError(f'{name} runner returned {rc}; no retry permitted')
        actual = ROOT/name
        assert not (actual/'ABORTED.json').exists()
        assert digest(actual/'plan.json') == segment['plan_sha256']
        assert read(actual/'schedule.json') == read(ROOT/segment['schedule'])
        done = read(actual/'complete.json')
        assert done['completed'] == done['scheduled'] == 8
        assert done['binaries'] == manifest['input_sha256']
        check_zero_retry_segment(actual, read(ROOT/segment['schedule']))
        completed.append(name)
        write(ROOT/'progress.json', {'completed_segments':completed,'completed_runs':8*len(completed)})
        print(f'COMPLETE {name} ({len(completed)}/80 segments; {8*len(completed)}/640 processes) {utc()}', flush=True)
    assert len(completed) == 80
    assert digest(manifest_path) == manifest_sha
    for path, expected in manifest['input_sha256'].items():
        assert digest(path) == expected, path
    write(ROOT/'complete.json', {'schema':'r17-acquisition-v1','completed_segments':80,
        'scheduled_segments':80,'completed_runs':640,'execution_manifest_sha256':manifest_sha,'finished_utc':utc()})
    print('R17 ACQUISITION COMPLETE; all 640 processes. Analysis may now begin.', flush=True)
except BaseException as exc:
    stop_active()
    write(ROOT/'ABORTED.json', {'schema':'r17-acquisition-v1','completed_segments':completed,
        'error_type':type(exc).__name__,'error':str(exc),'finished_utc':utc(),
        'execution_manifest_sha256':manifest_sha,'no_retry':'A separately registered restart decision is required'})
    raise
finally:
    stop_active()
