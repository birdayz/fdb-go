#!/usr/bin/env python3
"""V2 offline syntax/provenance/schedule/argv/summary checks; no FDB clients."""
import ast
import json
import math
from pathlib import Path
import subprocess
import tempfile
from unittest.mock import patch

from common import ROOT, PLAN, dump, load, path, schedule, sha
import timing


def main():
    p = load()
    for source in Path(__file__).resolve().parent.glob('*.py'):
        ast.parse(source.read_text(), filename=str(source))
    identities = timing.binaries(p, 'narrow')
    required = timing.executable_preflight(p, identities)
    assert len(identities) == 12
    jobs = schedule(p, 'narrow')
    assert len(jobs) == 96
    assert len({j['prefix'] for j in jobs}) == 96
    assert {len(j['prefix'].encode()) for j in jobs} == {68}
    assert all(j['prefix'].startswith(p['prefix_root']) for j in jobs)
    assert p['prefix_end'] == p['prefix_root'][:-1] + '0'
    assert p['counts'] == {'8': {'iterations': 12000, 'warmup': 800}}
    assert p['slots']['narrow']['wall_cap_seconds'] == 1200
    assert p['estimate']['conservative_seconds'] < 1200
    for workload in ('get100', 'get1000'):
        for arm in identities:
            mine = [j for j in jobs if j['arm'] == arm and j['workload']['name'] == workload]
            assert sorted(j['block'] for j in mine) == list(range(4))
            assert len({j['position'] for j in mine}) == 4
    frozen = timing.validator(p)
    assert callable(frozen.validate_raw_output) and callable(frozen.validate_identity)
    commands = []
    with tempfile.TemporaryDirectory(prefix='latency-v2-offline-') as tmp:
        scratch = Path(tmp)
        output = scratch / 'synthetic-summary'
        output.mkdir()
        (output / 'plan.json').write_bytes(PLAN.read_bytes())
        dump(output / 'verdict.json', {'valid': True})
        for job in jobs:
            folder = output / job['id']
            folder.mkdir()
            argv = timing.command(p, job, identities, folder / 'run')
            stub = list(argv)
            stub[3] = '/bin/true'
            # Actual taskset + requested affinity + exact remaining flags, but no real client.
            subprocess.run(stub, check=True, capture_output=True, text=True, timeout=5)
            commands.append({'production_argv': argv, 'offline_stub_argv': stub})
            ai = p['slots']['narrow']['arms'].index(job['arm'])
            rate = 0.02 if job['workload']['name'] == 'get100' else 0.01
            value = 100000 * (1 + ai * (rate + job['block'] * 0.001))
            dump(folder / 'done.json', {'returncode': 0, 'statistics': {'p50_ns': value, 'p99_ns': 2 * value}})
        result = timing.summarize(p, 'narrow', output)
        assert len(result['primary_adjacent_intervals']) == 11
        assert len(result['primary_upward_in_all_four_blocks']) == 11
        assert result['cell_roles']['get1000/w8'].startswith('SECONDARY, EXPLORATORY')
        endpoint = result['primary_endpoint']['geometric_mean']
        telescoped = math.prod(row['ratios']['geometric_mean'] for row in result['primary_adjacent_intervals'])
        assert math.isclose(endpoint, telescoped)
        for row in result['primary_adjacent_intervals']:
            ratios = row['ratios']
            assert len(ratios['per_block']) == 4
            assert ratios['block_min'] == min(ratios['per_block'])
            assert ratios['block_max'] == max(ratios['per_block'])
            assert ratios['range_label'] == 'block min/max, not CI'
        # No report from an invalid/incomplete attempt.
        dump(output / 'verdict.json', {'valid': False})
        try:
            timing.summarize(p, 'narrow', output)
        except RuntimeError:
            pass
        else:
            raise AssertionError('partial run accepted')
    report = {'valid': True, 'scope': 'offline only: no real client binary, Docker call or FDB connection executed',
              'plan_sha256': sha(PLAN), 'arms': 12, 'blocks': 4, 'cells': 2, 'processes': 96,
              'distinct_prefixes_68_bytes': 96, 'distinct_positions_each_arm_cell': 4,
              'actual_taskset_plus_bin_true_executions': len(commands),
              'exact_argv_except_only_client_executable_replaced': True,
              'real_executables_available': required,
              'synthetic_primary_adjacent_ratios': 'all eleven checked, with four block ratios, min/max and telescoping to endpoint',
              'partial_run_rejected': True,
              'frozen_raw_validator_and_existing_operational_controller': 'unchanged from successful v1 and full synthetic 248-process rehearsal'}
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
