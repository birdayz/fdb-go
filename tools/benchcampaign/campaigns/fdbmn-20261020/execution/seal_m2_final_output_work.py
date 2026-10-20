#!/usr/bin/env python3
"""Root4245: final M2 via unchanged existing sealer, output-scoped work only."""
import contextlib
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import sys

os.umask(0o077)
R = Path('/srv/fdb-bench')
prior = R / 'diagnostic/m2-v3'
out = R / 'plans/testingb'
source_template = R / 'provenance/p0-continuation-2/placement-command.json'
final_template = source_template.with_name('placement-command-output-work.json')


def sha(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def load(path):
    return json.loads(Path(path).read_text())


def dump(path, value):
    with Path(path).open('x') as stream:
        json.dump(value, stream, indent=2, sort_keys=True)
        stream.write('\n')


receipt = load(prior / 'plans/sealed.json')
manifest = load(prior / 'plans/manifest.json')
assert sha(prior / 'plans/manifest.json') == receipt['manifest_sha256']
assert manifest['pinned_files']
for path, expected in manifest['pinned_files'].items():
    assert sha(path) == expected, path
before = {p: sha(p) for p in (R/'protocol.json', R/'orders.json', R/'provenance/source-receipt.json')}
assert not out.exists() and not final_template.exists(), 'Fresh final output/template required'
v3_template = Path(receipt['placement_template'])
assert manifest['pinned_files'][str(v3_template)] == sha(v3_template)
assert manifest['pinned_files'][str(source_template)] == sha(source_template)
original, derived = load(source_template), load(v3_template)
assert isinstance(original, list) and original.count('--work-dir') == 1
index = original.index('--work-dir') + 1
assert index < len(original)
expected = list(original)
expected[index] = '{output}.work'
assert derived == expected, 'Only the work-dir value may differ from original P0 template'
assert receipt['diagnostic_plan_correction']['authorization'] == 'root4245'
dump(final_template, derived)
prior_call = receipt['sealer_calls'][-1]
argv = list(prior_call['argv'])
assert argv[0] == 'testingb-plans'
for option, value in (('--protocol', R/'protocol.json'), ('--orders', R/'orders.json'), ('--output-dir', out)):
    assert argv.count(option) == 1
    argv[argv.index(option) + 1] = str(value)
replaced = 0
for i, option in enumerate(argv[:-1]):
    if option == '--pin' and argv[i + 1] == str(v3_template):
        argv[i + 1] = str(final_template)
        replaced += 1
assert replaced == 1, 'Expected exactly one diagnostic derived-template pin'
sys.path.insert(0, str(R/'execution/placement'))
sys.path.insert(0, str(R/'tooling/tools/benchcampaign/multinode'))
import seal_multinode as sm
import seal_live as sl
stdout = io.StringIO()
with contextlib.redirect_stdout(stdout):
    rc = sm.main(argv)
assert rc == 0, stdout.getvalue()
mp = out / 'manifest.json'
final_manifest = load(mp)
protocol = load(R/'protocol.json')
plan_hashes = sl.validate_manifest('testingb-final', final_manifest, out, protocol)
assert final_manifest['pinned_files'][str(final_template)] == sha(final_template)
for path, expected in final_manifest['pinned_files'].items():
    assert sha(path) == expected, path
for path, expected in before.items():
    assert sha(path) == expected, path
result = copy.deepcopy(receipt)
result.pop('diagnostic_plan_correction', None)
result.update(mode='testingb-final', diagnostic_not_campaign_results=False,
              manifest=str(mp), manifest_sha256=sha(mp), protocol=str(R/'protocol.json'),
              protocol_sha256=sha(R/'protocol.json'), orders=str(R/'orders.json'),
              orders_sha256=sha(R/'orders.json'), live_protocol_sha256=sha(R/'protocol.json'),
              results={'M2rl': str(R/'results/M2rl'), 'M2skip': str(R/'results/M2skip')},
              plans_sha256=plan_hashes, phases=final_manifest['phases'], unsealed=final_manifest['unsealed'],
              pinned_files_count=len(final_manifest['pinned_files']),
              sealer_calls=[{'argv': argv, 'returncode': rc, 'stdout': stdout.getvalue()}],
              placement_template=str(final_template), placement_template_sha256=sha(final_template),
              placement_workdir_correction={'authorization': 'root4245', 'source_template': str(source_template),
                  'source_template_sha256': sha(source_template),
                  'change': 'only --work-dir to {output}.work, additional/replaced derived-template pin',
                  'live_protocol_unchanged': True, 'frozen_tools_unchanged': True})
dump(out/'sealed.json', result)
print(json.dumps({k: result[k] for k in ('ok','mode','manifest','manifest_sha256','protocol','orders','results','placement_template')}, sort_keys=True))
