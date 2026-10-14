#!/usr/bin/env python3
"""Freeze an N-arm clientbench campaign plan (2..6 arms, CGo control included).

Each Go arm is a clientbench executable built from its own declared source
revision; the CGo arm is the native control. Arms share every option and the
CPU affinity; only the binary, its declared revision and the key prefix differ,
and arm names are padded so every prefix has the same length. Blocks must be a
multiple of factorial(arms), so run.py runs every arm order equally often.
The R3 settings (seed, durations, quiescence limits, environment, 180-second
campaign cap) are kept; workload iterations and blocks are explicit.
"""
import argparse
import json
import math
from pathlib import Path
import re
import uuid

from r3_plan import cpus, digest


def parse_arm(value):
    """NAME=BINARY@REVISION; the CGo arm is named cgo."""
    name, sep, rest = value.partition('=')
    binary, sep2, revision = rest.rpartition('@')
    if not sep or not sep2 or not name or not binary:
        raise ValueError('arm must be NAME=BINARY@REVISION: ' + value)
    if not re.fullmatch(r'[A-Za-z0-9_-]+', name):
        raise ValueError('unsafe arm name: ' + name)
    if not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise ValueError('arm revision must be a full Git commit SHA: ' + value)
    return name, binary, revision


def make_plan(args):
    arms = [parse_arm(a) for a in args.arm]
    names = [a[0] for a in arms]
    if not 2 <= len(arms) <= 6 or len(set(names)) != len(names) or 'cgo' not in names:
        raise ValueError('2..6 uniquely named arms including cgo required')
    if len({str(Path(a[1]).resolve()) for a in arms}) != len(arms):
        raise ValueError('every arm needs its own binary')
    if args.blocks < 2 or args.blocks % math.factorial(len(arms)):
        raise ValueError(f'blocks must be a positive multiple of {math.factorial(len(arms))}')
    if not args.workload or len(set(args.workload)) != len(args.workload):
        raise ValueError('one or more distinct workloads required')
    if args.iterations <= 0 or args.warmup < 0:
        raise ValueError('positive iterations and nonnegative warmup required')
    if not re.fullmatch(r'[0-9a-f]{64}', args.harness_id):
        raise ValueError('harness-id must be the harness source archive SHA256')
    if not re.fullmatch(r'[0-9a-f]{29}', args.campaign_id):
        raise ValueError('campaign-id must be 29 lowercase hex digits')
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
                                              here / 'run.py', here / 'analyze.py', here / 'r3_plan.py',
                                              here / 'matrix_plan.py', *args.pin]}
    width = max(3, *(len(n) for n in names))
    go_revisions = {revision for name, _, revision in arms if name != 'cgo'}
    plan = dict(campaign='mx-' + args.campaign_id, source_commit=sorted(go_revisions)[0],
                harness_id=args.harness_id, seed=args.seed, blocks=args.blocks,
                cooldown_seconds=2, quiescence_seconds=1, max_busy_fraction=.15,
                max_steal_fraction=.01, telemetry_seconds=1, cpu_sets=groups,
                timeout_seconds=180, campaign_wall_cap_seconds=180,
                environment=dict(GOMAXPROCS=str(args.gomaxprocs), GOGC='100', GOMEMLIMIT='off', GODEBUG='',
                                 LC_ALL='C', TZ='UTC'),
                workloads=[dict(name=w, iterations=args.iterations, warmup=args.warmup, workers=1)
                           for w in args.workload],
                go_version='go1.26.9', fdb_version='7.3.77', fdb_api_version=730,
                native_library_sha256=digest(args.native_library), pinned_files=sorted(pinned), arms=[])
    for name, binary, revision in arms:
        binary = str(Path(binary).resolve())
        command = ['taskset', '--cpu-list', args.client_cpus, '{binary}', '--cluster-file',
                   str(Path(args.cluster_file).resolve()),
                   '--workload', '{workload}', '--iterations', '{iterations}', '--warmup', '{warmup}',
                   '--workers', '{workers}', '--output', '{output}', '--run-id', '{run_id}', '--block', '{block}',
                   '--source-revision', revision, '--harness-id', '{harness_id}', '--allow-writes',
                   '--transaction-timeout-ms', '30000', '--retry-limit', '10', '--prefix',
                   'clientbench/' + args.campaign_id + '/{workload:_<16}/b{block:03d}/' + name.ljust(width, '_') + '/']
        plan['arms'].append(dict(name=name, binary=binary, source_revision=revision, command=command))
        pinned.add(binary)
    plan['expected_sha256'] = {p: digest(p) for p in sorted(pinned)}
    if args.harness_id not in {plan['expected_sha256'][str(Path(p).resolve())] for p in args.pin}:
        raise ValueError('harness-id must match an explicitly pinned harness archive')
    return plan


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--arm', action='append', required=True, help='NAME=BINARY@REVISION; repeat; one arm named cgo')
    ap.add_argument('--workload', action='append', required=True)
    ap.add_argument('--iterations', type=int, required=True)
    ap.add_argument('--warmup', type=int, required=True)
    ap.add_argument('--blocks', type=int, required=True)
    for name in ['cluster-file', 'native-library', 'harness-id']:
        ap.add_argument('--' + name, required=True)
    ap.add_argument('--server-binary', default='/usr/sbin/fdbserver')
    ap.add_argument('--pin', action='append', default=[], help='additional immutable source/configuration artifact to hash')
    ap.add_argument('--campaign-id', default=uuid.uuid4().hex[:29])
    ap.add_argument('--seed', type=int, default=641001)
    ap.add_argument('--gomaxprocs', type=int, default=8)
    ap.add_argument('--housekeeping-cpus', required=True)
    ap.add_argument('--server-cpus', required=True)
    ap.add_argument('--client-cpus', required=True)
    args = ap.parse_args()
    print(json.dumps(make_plan(args), indent=2, sort_keys=True))


if __name__ == '__main__':
    main()
