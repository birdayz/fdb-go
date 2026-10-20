#!/usr/bin/env python3
"""Create all 80 registered plans/schedules without launching a benchmark."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path

P = Path('/opt/fdbperf/artifacts/fullstack-r17-build')
ROOT = Path('/opt/fdbperf/artifacts/fullstack-r17-timing')
SCRIPTS = Path('/opt/fdbperf/sources/r17-frozen-harness/tools/clientbench/scripts')
R16 = Path('/opt/fdbperf/artifacts/fullstack-r16-build')
V7 = R16 / 'reused-v7-build'
BASE = '7922a2e5a8e3a2d6cf17dfee8b8134181c88b298'
PATCH = 'd0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85'
NATIVE_ORIGIN = '8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead'

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()

def write(path, value):
    with path.open('x') as stream:
        stream.write(json.dumps(value, indent=2, sort_keys=True) + '\n')

for filename, expected in {
    'analyze.py':'5d3086d28e7e856f4b9f9033e6f6790e1e90031fa2efe19b122b27b731fe577b',
    'run.py':'7ce9e96c4e736b2df559f62b00c4eacfd4ad4b1623ca380d94e07896d96669f2',
    'r3_plan.py':'effa82052b4327d0998b4c914250f4bd1e66e22397003b818d55f4892904f9d3',
}.items():
    assert digest(SCRIPTS/filename) == expected, filename
generator = load('r17_generator', SCRIPTS / 'r3_plan.py')
runner = load('r17_runner', SCRIPTS / 'run.py')
assert ROOT.is_dir() and not (ROOT / 'execution-manifest.json').exists()
prereg = ROOT / 'R17-preregistration-v1.md'
addendum = ROOT / 'R17-preregistration-addendum-v2.md'
assert digest(prereg) == '2c395126fc090504c21ae7fb4e1ca513aa1727c6082d55e488af851c9c616788'
assert digest(addendum) == '75bb7c31ef30e3133013eaed5a301ae8ea56a41d8924cd8dab14807583984dad'
assert digest('/opt/fdbperf/bin/clientbench-r16-new') == '93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f'
assert digest('/opt/fdbperf/bin/clientbench-v7-cgo') == '4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c'
assert digest('/usr/lib/libfdb_c.so') == 'e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1'
harness = V7 / 'harness-v7-candidate.tar.gz'
assert digest(harness) == '48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203'
assert digest(V7/'standalone-wrapper-v7.tar.gz') == '8f340663085b961230046bf440e1e80df27e24976219fe3b6e3ae6a1193f1a6a'
assert digest(R16/'fullstack-on-7922a2e5a.patch') == PATCH
assert digest('/usr/sbin/fdbserver') == '179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f'
pins = [prereg, addendum, harness, V7/'standalone-wrapper-v7.tar.gz', V7/'parent-full.tar.gz',
        V7/'review-acks.json', V7/'review-candidate-sha.txt', R16/'parent-full.tar.gz',
        R16/'parent-git-tree.txt', R16/'root-module-pins-equivalence.patch',
        R16/'fullstack-on-7922a2e5a.patch', R16/'source-delta.json', R16/'binaries.sha256',
        R16/'full-new.sha256', R16/'new-go-version-m.txt',
        Path('/etc/foundationdb/foundationdb.conf'), Path('/usr/lib/foundationdb/fdbmonitor'),
        Path('/etc/systemd/system/foundationdb.service'),
        Path('/etc/systemd/system/foundationdb.service.d/affinity.conf'),
        Path('/opt/fdbperf/scripts/setup-r17.sh'), Path('/opt/fdbperf/scripts/finish-setup-r17.sh')]
pins += [P/name for name in ['prepare-r17.py','acquire-r17.py','analyze-r17.py','r17_statistics.py','selfcheck-r17.py',
          'runtime.sha256','setup-status.txt','os-release.txt','lscpu.txt','cpu-topology.txt',
          'boot-id.txt','kernel-command-line.txt','fdb-service.txt']]
pins += sorted((P/'rebuild-evidence').glob('*.json'))
(ROOT/'plans').mkdir()
segments = []
all_prefixes = []
input_sha = None
for index in range(80):
    name = f'seg{index:03d}'
    args = argparse.Namespace(go_binary=Path('/opt/fdbperf/bin/clientbench-r16-new'),
        cgo_binary=Path('/opt/fdbperf/bin/clientbench-v7-cgo'),
        cluster_file=Path('/etc/foundationdb/fdb.cluster'), native_library=Path('/usr/lib/libfdb_c.so'),
        server_binary=Path('/usr/sbin/fdbserver'), source_revision=BASE, harness_id=digest(harness),
        campaign_id='b717'+format(index,'025x'), housekeeping_cpus='0-1', server_cpus='2-7',
        client_cpus='8-15', pin=pins)
    plan = generator.make_plan(args)
    new, cgo = plan['arms']
    new['name'] = 'new'
    new['source_revision'] = BASE + '.patch.' + PATCH
    new['command'] = [value.replace('/go_/', '/new/') for value in new['command']]
    new['command'][new['command'].index('--source-revision')+1] = new['source_revision']
    cgo['source_revision'] = NATIVE_ORIGIN
    cgo['command'][cgo['command'].index('--source-revision')+1] = NATIVE_ORIGIN
    plan.update(campaign='fullstack-r17-'+name, blocks=2, seed=797001+index,
        workloads=[dict(name='read1000',iterations=5000,warmup=200,workers=1),
                   dict(name='batchset50',iterations=2500,warmup=200,workers=1)],
        harness_version='V7.06097ca4609b1b927a9827f4fff5e8cedf358e46',
        purpose='R17 fixed confirmatory NEW/CGo; exact R16 stack; no interim performance analysis',
        global_block_offset=2*index)
    paths = set(plan['pinned_files']) | {arm['binary'] for arm in plan['arms']}
    plan['expected_sha256'] = {path:digest(path) for path in sorted(paths)}
    if input_sha is None:
        input_sha = plan['expected_sha256']
    assert plan['expected_sha256'] == input_sha
    schedule = runner.make_schedule(plan, ROOT/name)
    assert len(schedule) == 8
    plan_path, schedule_path = ROOT/'plans'/(name+'-plan.json'), ROOT/'plans'/(name+'-schedule.json')
    write(plan_path, plan)
    write(schedule_path, schedule)
    for row in schedule:
        command = row['command']
        all_prefixes.append(command[command.index('--prefix')+1])
    segments.append(dict(index=index, name=name, global_block_offset=2*index,
        plan=str(plan_path.relative_to(ROOT)), schedule=str(schedule_path.relative_to(ROOT)),
        plan_sha256=digest(plan_path), schedule_sha256=digest(schedule_path)))
assert len(all_prefixes) == len(set(all_prefixes)) == 640
assert {len(prefix.encode()) for prefix in all_prefixes} == {68}
manifest = dict(schema='r17-acquisition-v1', blocks_per_workload=160, scheduled_segments=80,
    scheduled_runs=640, segments=segments, preregistration_sha256=digest(prereg),
    addendum_sha256=digest(addendum), input_sha256=input_sha)
write(ROOT/'execution-manifest.json', manifest)
print('Frozen all 80 segments / 640 processes / 160 pairs per workload before acquisition')
print('execution_manifest_sha256='+digest(ROOT/'execution-manifest.json'))
