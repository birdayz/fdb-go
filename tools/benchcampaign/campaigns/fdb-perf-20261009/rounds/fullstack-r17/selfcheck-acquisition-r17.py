#!/usr/bin/env python3
"""Reproduce the 152 artifact-only R17 acquisition-gate mutations; no acquisition."""
import ast
import copy
import csv
import hashlib
import json
from pathlib import Path
import tempfile


def main():
    work = Path(__file__).resolve().parent
    source = work / 'acquire-r17.py'
    expected = 'f3bbcae599ebe24d517258cdbd9bb1441d2db0eaf8cb4957cdf481a869c66159'
    assert hashlib.sha256(source.read_bytes()).hexdigest() == expected, 'reviewed acquisition source changed'
    tree = ast.parse(source.read_text())
    nodes = [node for node in tree.body if isinstance(node, ast.FunctionDef)
             and node.name in ('read', 'check_zero_retry_segment')]
    assert len(nodes) == 2
    scope = {'csv': csv, 'json': json}
    exec(compile(ast.Module(body=nodes, type_ignores=[]), 'acquire-r17.py:two-read-only-functions', 'exec'), scope)
    check = scope['check_zero_retry_segment']
    cases = 0
    with tempfile.TemporaryDirectory(prefix='zero-retry-check-', dir=work) as temp:
        root = Path(temp)
        runs, originals = [], {}
        for block in range(2):
            for workload, n in [('read1000', 5000), ('batchset50', 2500)]:
                for arm in ['new', 'cgo']:
                    rid = f'b{block:03d}-{workload}-{arm}'
                    runs.append({'run_id': rid, 'arm': arm, 'workload': {'name': workload, 'iterations': n}})
                    folder = root / rid
                    folder.mkdir()
                    summary = {
                        'errors': 0, 'warmup_errors': 0, 'retries': 0, 'warmup_retries': 0,
                        'failure': '', 'warmup_requested': 200,
                        'client_counters_delta': {'values': {
                            'transaction_retries': 0, 'connection_failures': 0, 'grv_cache_hits': 0,
                            'read_versions_completed': n, 'grv_successful_batch_replies': n,
                            'commit_started': 0 if workload == 'read1000' else n,
                            'commit_completed': 0 if workload == 'read1000' else n,
                        }},
                    }
                    originals[rid] = summary
                    (folder / 'summary.json').write_text(json.dumps(summary))
                    for filename, count in [('samples.csv', n), ('warmup.csv', 200)]:
                        (folder / filename).write_text('attempts\n' + '1\n' * count)
        check(root, runs)
        for run in runs:
            folder = root / run['run_id']
            base = originals[run['run_id']]

            def rejects(summary=None, filename=None, text=None):
                nonlocal cases
                old = None
                if summary is not None:
                    (folder / 'summary.json').write_text(json.dumps(summary))
                if filename is not None:
                    old = (folder / filename).read_text()
                    (folder / filename).write_text(text)
                try:
                    check(root, [run])
                except (AssertionError, KeyError, ValueError):
                    cases += 1
                else:
                    raise AssertionError(('mutation accepted', run, summary, filename))
                finally:
                    (folder / 'summary.json').write_text(json.dumps(base))
                    if filename is not None:
                        (folder / filename).write_text(old)

            for key in ('errors', 'warmup_errors', 'retries', 'warmup_retries'):
                for bad in (1, False):
                    mutated = copy.deepcopy(base)
                    mutated[key] = bad
                    rejects(summary=mutated)
            for key, bad in [('failure', 'bad'), ('warmup_requested', 201)]:
                mutated = copy.deepcopy(base)
                mutated[key] = bad
                rejects(summary=mutated)
            for filename, count in [('samples.csv', run['workload']['iterations']), ('warmup.csv', 200)]:
                rejects(filename=filename, text='attempts\n' + '1\n' * (count - 1))
                rejects(filename=filename, text='attempts\n2\n' + '1\n' * (count - 1))
            if run['arm'] == 'new':
                for key in ('transaction_retries', 'connection_failures', 'grv_cache_hits'):
                    for bad in (1, False):
                        mutated = copy.deepcopy(base)
                        mutated['client_counters_delta']['values'][key] = bad
                        rejects(summary=mutated)
                for key in ('read_versions_completed', 'grv_successful_batch_replies', 'commit_started', 'commit_completed'):
                    mutated = copy.deepcopy(base)
                    mutated['client_counters_delta']['values'][key] += 1
                    rejects(summary=mutated)
        check(root, runs)
    assert cases == 152
    print('PASS: valid eight-run synthetic segment; 152 invalid mutations rejected; restored valid segment passes.')
    print('Only AST-extracted read/check functions executed; no acquisition, subprocess, network, real metrics, or repository actions.')


if __name__ == '__main__':
    main()
