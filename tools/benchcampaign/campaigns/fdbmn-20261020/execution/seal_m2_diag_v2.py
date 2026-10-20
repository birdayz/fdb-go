#!/usr/bin/env python3
"""Root4236: existing sealer calls, fresh diagnostic-only 3-repeat/1x plan."""
import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import sys

R = Path('/srv/fdb-bench')
old = R / 'diagnostic/m2'
new = R / 'diagnostic/m2-v2'

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
for path, expected in manifest['pinned_files'].items():
    assert sha(path) == expected, path
before = {p: sha(p) for p in (R/'protocol.json', R/'orders.json', R/'provenance/source-receipt.json')}
assert not new.exists()
protocol = load(old / 'protocol.json')
assert protocol['diagnostic']['not_campaign_results'] is True
assert len(protocol['phases']) == 1
phase = protocol['phases'][0]
assert phase['id'] == 'M2skip' and phase['blocks'] == 2
assert phase['repeats_per_arm_process'] == 1 and phase['benchtime'] == '1x'
protocol['campaign'] += '-v2'
phase['repeats_per_arm_process'] = 3
new.mkdir()
dump(new / 'protocol.json', protocol)
sys.path.insert(0, str(R/'execution/placement'))
sys.path.insert(0, str(R/'tooling/tools/benchcampaign/multinode'))
import seal_multinode as sm
import seal_live as sl
calls = []
assert len(receipt['sealer_calls']) == 3
for prior in receipt['sealer_calls']:
    argv = [arg.replace(str(old), str(new)) for arg in prior['argv']]
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
for path, expected in before.items():
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
              diagnostic_plan_correction={'authorization': 'root4236', 'prior_failed_workspace': str(old),
                  'change': 'diagnostic repeats1 to3, same1x; existing sealer functions only',
                  'live_protocol_unchanged': True, 'frozen_tools_unchanged': True})
dump(new/'plans/sealed.json', result)
print(json.dumps({k: result[k] for k in ('ok','manifest','manifest_sha256','protocol','orders','results')}, sort_keys=True))
