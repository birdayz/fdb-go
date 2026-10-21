#!/usr/bin/env python3
"""Freeze the R3 two-arm, six-block batchset50 design for a fresh campaign."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import uuid


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def cpus(value):
    result = []
    for part in value.split(','):
        ends = part.split('-')
        if len(ends) == 1:
            result.append(int(ends[0]))
        elif len(ends) == 2 and int(ends[0]) <= int(ends[1]):
            result.extend(range(int(ends[0]), int(ends[1]) + 1))
        else:
            raise ValueError('invalid CPU list: ' + value)
    if not result or min(result) < 0 or len(result) != len(set(result)):
        raise ValueError('empty, negative or duplicate CPU list')
    return result


def make_plan(args):
    if not re.fullmatch(r'[0-9a-f]{40}', args.source_revision):
        raise ValueError('source-revision must be a full Git commit SHA')
    if not re.fullmatch(r'[0-9a-f]{64}', args.harness_id):
        raise ValueError('harness-id must be the harness source archive SHA256')
    if not re.fullmatch(r'[0-9a-f]{29}', args.campaign_id):
        raise ValueError('campaign-id must be 29 lowercase hex digits (preserves 68-byte prefixes)')
    groups = [
        dict(name='housekeeping', cpus=cpus(args.housekeeping_cpus), max_busy_fraction=.30, max_cpu_busy_fraction=.80),
        dict(name='server', cpus=cpus(args.server_cpus), max_busy_fraction=.15, max_cpu_busy_fraction=.50),
        dict(name='client', cpus=cpus(args.client_cpus), max_busy_fraction=.05, max_cpu_busy_fraction=.15),
    ]
    population = [cpu for group in groups for cpu in group['cpus']]
    if len(population) != len(set(population)):
        raise ValueError('CPU groups must not overlap')
    here = Path(__file__).resolve().parent
    pinned = {str(Path(p).resolve()) for p in [args.cluster_file, args.native_library, args.server_binary,
                                              here / 'run.py', here / 'analyze.py', here / 'r3_plan.py', *args.pin]}
    plan = dict(campaign='r3-' + args.campaign_id, source_commit=args.source_revision,
                harness_id=args.harness_id, seed=641001, blocks=6,
                cooldown_seconds=2, quiescence_seconds=1, max_busy_fraction=.15,
                max_steal_fraction=.01, telemetry_seconds=1, cpu_sets=groups,
                timeout_seconds=180, campaign_wall_cap_seconds=180,
                environment=dict(GOMAXPROCS='8', GOGC='100', GOMEMLIMIT='off', GODEBUG='', LC_ALL='C', TZ='UTC'),
                workloads=[dict(name='batchset50', iterations=3000, warmup=200, workers=1)],
                go_version='go1.27.3', fdb_version='7.3.77', fdb_api_version=730,
                native_library_sha256=digest(args.native_library), pinned_files=sorted(pinned), arms=[])
    for arm, binary in [('go', args.go_binary), ('cgo', args.cgo_binary)]:
        binary = str(Path(binary).resolve())
        command = ['taskset', '--cpu-list', args.client_cpus, '{binary}', '--cluster-file', str(Path(args.cluster_file).resolve()),
                   '--workload', '{workload}', '--iterations', '{iterations}', '--warmup', '{warmup}',
                   '--workers', '{workers}', '--output', '{output}', '--run-id', '{run_id}', '--block', '{block}',
                   '--source-revision', args.source_revision, '--harness-id', '{harness_id}', '--allow-writes',
                   '--transaction-timeout-ms', '30000', '--retry-limit', '10', '--prefix',
                   'clientbench/' + args.campaign_id + '/{workload:_<16}/b{block:03d}/' + f'{arm:_<3}/']
        plan['arms'].append(dict(name=arm, binary=binary, source_revision=args.source_revision, command=command))
        pinned.add(binary)
    if len({a['binary'] for a in plan['arms']}) != 2:
        raise ValueError('Go and CGo must use different binaries')
    plan['expected_sha256'] = {p: digest(p) for p in sorted(pinned)}
    if args.harness_id not in {plan['expected_sha256'][str(Path(p).resolve())] for p in args.pin}:
        raise ValueError('harness-id must match an explicitly pinned harness archive')
    return plan


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    for name in ['go-binary', 'cgo-binary', 'cluster-file', 'native-library', 'source-revision', 'harness-id']:
        ap.add_argument('--' + name, required=True)
    ap.add_argument('--server-binary', default='/usr/sbin/fdbserver')
    ap.add_argument('--pin', action='append', default=[], help='additional immutable source/configuration artifact to hash')
    ap.add_argument('--campaign-id', default=uuid.uuid4().hex[:29])
    ap.add_argument('--housekeeping-cpus', default='0-1')
    ap.add_argument('--server-cpus', default='2-7')
    ap.add_argument('--client-cpus', default='8-15')
    args = ap.parse_args()
    print(json.dumps(make_plan(args), indent=2, sort_keys=True))


if __name__ == '__main__':
    main()
