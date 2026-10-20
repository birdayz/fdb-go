#!/usr/bin/env python3
"""Campaign phase boundary: existing P0 command/status/settle functions only."""
import hashlib
import json
import os
from pathlib import Path
import sys
from types import SimpleNamespace

os.umask(0o077)
R = Path('/srv/fdb-bench')
manifest = json.loads((R/'plans/client/manifest.json').read_text())
assert hashlib.sha256((R/'plans/client/manifest.json').read_bytes()).hexdigest() == '0a329eac7a1d366bf53cf5385056cb50f486bdf424b9354b96bd8ada4461c334'
for name, expected in manifest['pinned_files'].items():
    with Path(name).open('rb') as stream:
        assert hashlib.file_digest(stream, 'sha256').hexdigest() == expected, name
sys.path.insert(0, str(R/'execution/placement'))
import p0
args = SimpleNamespace(root=str(R), output=str(R/'provenance/m1-dd-on'), fdbcli='/usr/bin/fdbcli',
    expect_cluster_sha256='69e1731e91d6532a01e2cf41c5ee8716d33083a379b78909a1bf5c81671bd943',
    expect_placementprobe_sha256='1af0b563f4169d116a7c975bb2a15e9323853d267c219ce1ddd7a75bad5387cc',
    expect_fetch_sha256='360f03d66a4afa217db03359b8f657ee43a288eb7a42150c1c615323af1af118',
    expect_planskip_sha256='b2020a2044a5feba3e674d5e12a26ed76ff5b114be1bd38aa74491f5d53c29a4',
    settle_timeout_seconds=1200)
p = p0.P0(args)
folder = p0.mkdir(p.out)
p0.dump(folder/'invocation.json', {'purpose':'M2 to M1 phase boundary; no further M2 afterward',
    'no_user_key_mutation':True, 'existing_p0_functions_only':True, 'pinned_hashes':p.pins})
try:
    before, _ = p.status(folder, 'before', 'disabled')
    p.settle_expected_roles = p.cg.role_identity(before)
    p.cli_exec(folder, 'dd-on', 'datadistribution on')
    p.settle(folder)
    after, movement = p.status(folder, 'after', 'enabled')
    p0.need(not movement and p.cg.role_identity(after) == p.settle_expected_roles, 'post-settle movement/roles changed')
    p.check_pins()
    result = {'ok':True, 'dd_enabled':True, 'no_user_key_mutation':True,
        'settled':p0.load(folder/'settled.json'), 'roles':p.cg.role_identity(after), 'pinned_hashes':p.pins}
    p0.dump(folder/'complete.json', result)
    print(json.dumps({k:result[k] for k in ('ok','dd_enabled','no_user_key_mutation','settled')}, sort_keys=True))
except BaseException as exc:
    p0.dump(folder/'failure.json', {'ok':False,'error':repr(exc),'continue_to_m1':False})
    raise
