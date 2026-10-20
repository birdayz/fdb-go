#!/usr/bin/env python3
"""Existing sealer only: fresh M2 diagnostic with output-scoped placement work.

Preparation only until FDB_M2_WORKDIR_APPROVAL names the owner's root request.
"""
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import re
import sys

os.umask(0o077)
approval = os.environ.get('FDB_M2_WORKDIR_APPROVAL', '')
assert re.fullmatch(r'root[0-9]+', approval), 'Explicit root workdir-namespace approval required'
R = Path('/srv/fdb-bench')
old = R / 'diagnostic/m2-v2'
new = R / 'diagnostic/m2-v3'


def sha(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def load(path):
    return json.loads(Path(path).read_text())


def dump(path, value):
    with Path(path).open('x') as stream:
        json.dump(value, stream, indent=2, sort_keys=True)
        stream.write('\n')


receipt = load(old / 'plans/sealed.json')
manifest = load(old / 'plans/manifest.json')
assert sha(old / 'plans/manifest.json') == receipt['manifest_sha256']
assert manifest['pinned_files']
for path, expected in manifest['pinned_files'].items():
    assert sha(path) == expected, path
before = {p: sha(p) for p in (R/'protocol.json', R/'orders.json', R/'provenance/source-receipt.json')}
prior_files = {p: sha(p) for p in old.rglob('*') if p.is_file()}
assert not new.exists()
protocol = load(old / 'protocol.json')
assert protocol['diagnostic']['not_campaign_results'] is True
assert len(protocol['phases']) == 1
phase = protocol['phases'][0]
assert phase['id'] == 'M2skip' and phase['blocks'] == 2
assert phase['repeats_per_arm_process'] == 3 and phase['benchtime'] == '1x'
assert protocol['campaign'].endswith('-v2')
protocol['campaign'] = protocol['campaign'][:-3] + '-v3'
templates = [p for p in manifest['pinned_files'] if p.endswith('/placement-command.json')]
assert len(templates) == 1, 'Expected one original pinned P0 placement template'
source_template = templates[0]
template = load(source_template)
assert isinstance(template, list) and all(isinstance(x, str) for x in template)
assert template.count('--work-dir') == 1
index = template.index('--work-dir') + 1
assert index < len(template) and Path(template[index]).is_absolute()
derived = list(template)
derived[index] = '{output}.work'
new.mkdir(mode=0o700)
dump(new / 'protocol.json', protocol)
template_path = new / 'placement-command.json'
dump(template_path, derived)
sys.path.insert(0, str(R/'execution/placement'))
sys.path.insert(0, str(R/'tooling/tools/benchcampaign/multinode'))
import seal_multinode as sm
import seal_live as sl
calls = []
assert [call['argv'][0] for call in receipt['sealer_calls']] == ['orders', 'check-orders', 'testingb-plans']
for prior in receipt['sealer_calls']:
    argv = [arg.replace(str(old), str(new)) for arg in prior['argv']]
    if argv[0] == 'testingb-plans':
        argv += ['--pin', str(template_path)]
    output = io.StringIO()
    with contextlib.redirect_stdout(output):
        rc = sm.main(argv)
    calls.append({'argv': argv, 'returncode': rc, 'stdout': output.getvalue()})
    assert rc == 0, calls[-1]
mp = new/'plans/manifest.json'
new_manifest = load(mp)
plan_hashes = sl.validate_manifest('testingb-diagnostic', new_manifest, new/'plans', protocol)
for path, expected in new_manifest['pinned_files'].items():
    assert sha(path) == expected, path
assert new_manifest['pinned_files'][str(template_path)] == sha(template_path)
for path, expected in (before | prior_files).items():
    assert sha(path) == expected, path
plan = load(new/'plans/M2skip.plan.json')
assert len(plan['jobs']) == 4 and len(plan['cases']) == 1 and len(plan['orders']) == 2
assert all('-test.count=3' in j['command'] and '-test.benchtime=1x' in j['command'] for j in plan['jobs'])
result = copy.deepcopy(receipt)
result.update(manifest=str(mp), manifest_sha256=sha(mp), protocol=str(new/'protocol.json'),
              protocol_sha256=sha(new/'protocol.json'), orders=str(new/'orders.json'),
              orders_sha256=sha(new/'orders.json'), results={'M2skip': str(new/'results/M2skip')},
              plans_sha256=plan_hashes, phases=new_manifest['phases'],
              pinned_files_count=len(new_manifest['pinned_files']), sealer_calls=calls,
              placement_template=str(template_path), placement_template_sha256=sha(template_path),
              diagnostic_plan_correction={'authorization': approval, 'count_authorization': 'root4236',
                  'prior_failed_workspace': str(old), 'source_template': source_template,
                  'source_template_sha256': sha(source_template),
                  'change': 'placement work-dir only to {output}.work; same3repeats/1x; existing sealer functions only',
                  'live_protocol_unchanged': True, 'frozen_tools_unchanged': True})
dump(new/'plans/sealed.json', result)
print(json.dumps({k: result[k] for k in ('ok','manifest','manifest_sha256','protocol','orders','results','placement_template')}, sort_keys=True))
