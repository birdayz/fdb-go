#!/usr/bin/env python3
"""Register all four V7 campaigns before any measurement; do not adapt budgets."""
import argparse
import importlib.util
import json
from pathlib import Path

p = Path('/opt/fdbperf/artifacts/lifecycle-v7-build')
scripts = Path('/opt/fdbperf/sources/lifecycle-v7-baseline/tools/clientbench/scripts')
spec = importlib.util.spec_from_file_location('r3_plan', scripts / 'r3_plan.py')
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)
sha = (p / 'review-candidate-sha.txt').read_text().strip()
base = '8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead'
stack = '999bc230aadbf6f5069b237d29bd4c6a690f30d015971ec175d417e2e0dd6316'
harness = p / 'harness-v7-candidate.tar.gz'
for comparison, work, iterations, seed, namespace in [
    ('baseline', 'batchset50', 3000, 641001, 'b701' + '0' * 25),
    ('baseline', 'read1000', 5000, 641101, 'b702' + '0' * 25),
    ('stack', 'batchset50', 3000, 641201, 'b703' + '0' * 25),
    ('stack', 'read1000', 5000, 641301, 'b704' + '0' * 25),
]:
    args = argparse.Namespace(go_binary=Path('/opt/fdbperf/bin/clientbench-v7-' + comparison + '-go'),
        cgo_binary=Path('/opt/fdbperf/bin/clientbench-v7-cgo'), cluster_file=Path('/etc/foundationdb/fdb.cluster'),
        native_library=Path('/usr/lib/libfdb_c.so'), server_binary=Path('/usr/sbin/fdbserver'), source_revision=base,
        harness_id=generator.digest(harness), campaign_id=namespace, housekeeping_cpus='0-1', server_cpus='2-7', client_cpus='8-15',
        pin=[harness, p/'standalone-wrapper-v7.tar.gz', p/'parent-full.tar.gz', p/'parent-git-tree.txt',
             p/'review-candidate-sha.txt', p/'review-acks.json', p/'binaries.sha256', p/'runtime.sha256',
             p/'full-baseline.sha256', p/'full-stack.sha256', p/'stack-pr3-pr4-pr5v2-on-8ab5c5a4d.patch',
             p/'fdb-go-range-iterator-done.patch', p/'prepare-v7.py', p/'run-v7.sh',
             Path('/etc/foundationdb/foundationdb.conf')])
    plan = generator.make_plan(args)
    plan['campaign'] = 'v7-' + comparison + '-' + work
    plan['harness_version'] = 'V7.' + sha
    plan['seed'] = seed
    plan['workloads'] = [dict(name=work, iterations=iterations, warmup=200, workers=1)]
    plan['purpose'] = ('Fresh lifecycle accounting, six two-arm blocks and three of each order. '
                       'Window CPU plus drained delta and paired post-identity CPU through terminal shutdown (E-A). Absolute E retained unpaired only. '
                       'Not isolated steady-state transaction CPU. Exact baseline ' + base +
                       ('; authoritative R11 stack patch ' + stack if comparison == 'stack' else '') +
                       '. The harness is V7; frozen V6 results are not mixed in.')
    plan['analysis'] = dict(seed=642001, resamples=10000, method='paired run-block percentile bootstrap',
        caveat='Six same-VM blocks; no physical-host independence, multiplicity correction or noninferiority/capacity claim.')
    if comparison == 'stack':
        arm = next(a for a in plan['arms'] if a['name'] == 'go')
        arm['source_revision'] = base + '.patch.' + stack
        arm['command'][arm['command'].index('--source-revision') + 1] = arm['source_revision']
    with (p / (plan['campaign'] + '-plan.json')).open('x') as f:
        f.write(json.dumps(plan, indent=2, sort_keys=True) + '\n')
