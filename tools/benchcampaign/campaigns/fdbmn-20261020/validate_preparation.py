#!/usr/bin/env python3
"""Validate private campaign preparation; cloud calls are not made by this file."""
import itertools
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile

C = Path(__file__).resolve().parent


def check_commands():
    protected = [line.split('\t') for line in (C / 'inventory/protect.tsv').read_text().splitlines()]
    assert len(protected) == 17
    ids = sorted(int(row[1]) for row in protected)
    names = sorted(row[2] for row in protected)
    for filename in ('teardown-deadline.sh', 'teardown-immediate.sh', 'teardown-report-only.sh'):
        text = (C / 'commands' / filename).read_text()
        argv = shlex.split(text.splitlines()[-1])
        assert sorted(int(argv[i+1]) for i, word in enumerate(argv) if word == '--protect-id') == ids
        assert sorted(argv[i+1] for i, word in enumerate(argv) if word == '--protect-name') == names
        assert argv[argv.index('--label')+1] == 'campaign=fdbmn-20261020-NONCE'
        assert argv[argv.index('--prefix')+1] == 'fdbmn-20261020-NONCE'
        if filename == 'teardown-deadline.sh':
            assert argv[argv.index('--deadline')+1] == '$DEADLINE' and '--execute' in argv
        if filename == 'teardown-report-only.sh':
            assert '--execute' not in argv
    for path in sorted((C / 'commands').glob('*.sh')):
        subprocess.run(['bash', '-n', str(path)], check=True)
    for path in sorted((C / 'commands').glob('provision-dry-run*.sh')):
        text = path.read_text()
        assert '--image "$IMAGE_ID"' in text and '[[ "$IMAGE_ID" =~ ^[0-9]+$ ]]' in text
        assert '--execute' not in text.splitlines()[-1]
        assert '--protected-cidr <existing-cidr>' in text
    print('PASS: command syntax, immutable image requirement, exact campaign identity and all 17 protected IDs/names.')


def check_teardown_refusals():
    # Import only the guard's pure classification function; no discover/delete.
    sys.path.insert(0, str(C / 'tooling/tools/benchcampaign/multinode'))
    import teardown_guard as guard
    protected = [line.split('\t') for line in (C / 'inventory/protect.tsv').read_text().splitlines()]
    args = guard.parse_args(['--label', 'campaign=fdbmn-20261020-NONCE', '--prefix', 'fdbmn-20261020-NONCE',
                            *[a for _, rid, name in protected for a in ('--protect-id', rid, '--protect-name', name)]])
    for kind, rid, name in protected:
        item = next(x for x in json.loads((C / f'inventory/before-{kind}.json').read_text()) if x['id'] == int(rid))
        owned, refused = guard.classify(args, kind, [item])
        assert not owned and len(refused) == 1
        assert {'label mismatch', 'name outside prefix', 'protected id', 'protected name'} <= set(refused[0]['reasons'])
        relabelled = dict(item, labels={'campaign': args.campaign})
        owned, refused = guard.classify(args, kind, [relabelled])
        assert not owned and {'protected id', 'protected name'} <= set(refused[0]['reasons'])
        id_only = dict(relabelled, name=args.prefix + '-control')
        assert guard.classify(args, kind, [id_only])[1][0]['reasons'] == ['protected id']
        name_only = dict(relabelled, id=999999999999)
        assert 'protected name' in guard.classify(args, kind, [name_only])[1][0]['reasons']
    for item, want in [({'id': 999999999999, 'name': args.prefix+'-control', 'labels': {}}, ['label mismatch']),
                       ({'id': 999999999999, 'name': 'unowned-control', 'labels': {'campaign': args.campaign}}, ['name outside prefix'])]:
        owned, refused = guard.classify(args, 'server', [item])
        assert not owned and refused[0]['reasons'] == want
    print('PASS: teardown classifier refuses all 17 protected resources, accidental relabels, isolated protections and ownership mismatches.')


def check_protocol():
    p = json.loads((C / 'protocol.json').read_text())
    orders = json.loads((C / 'orders.json').read_text())
    assert p['production_revisions']['final'] == 'TBD_AFTER_RFC_263_MERGE'
    assert p['production_revisions']['before_allocation_work_after_read_parity'] == '267a41434a81723b07e7150869d702636a1b91a9'
    assert p['phase_order']['default'] == ['gate-F0', 'gate-P0', 'M2rl', 'M2skip', 'DD-on-settle', 'M1w1', 'M1w64', 'M1w8', 'M1w32']
    for phase in p['phases']:
        o = orders[phase['frozen_order_key']]['orders']
        assert len(o) == phase['blocks'] == 6
        if phase['kind'] == 'client':
            assert sorted(map(tuple, o)) == sorted(itertools.permutations(phase['arms']))
            assert len(phase['workloads']) == 7
    budget = p['budget']
    fixed = budget['fixed_seconds']
    overhead = fixed['setup_gates_F0_P0_controls_capacity_smokes'] + fixed['per_phase_collection'] * 6 + fixed['final_teardown_verification']
    assert sum(budget['expected_phase_seconds'].values()) + overhead == budget['expected_total_seconds'] == 18825
    assert sum(budget['phase_caps_seconds'].values()) + overhead == budget['worst_case_total_seconds'] == 26400
    assert budget['lifetime_hours'] == 8
    print('PASS: six-permutation M1 blocks, six phases, pending merge SHA and recomputed budget.')


def check_inventory_failure_controls():
    # A fake hcloud executable reads fixtures only. It refuses any command other
    # than the verifier's exact read-only list shape; never invokes real hcloud.
    with tempfile.TemporaryDirectory(prefix='verify-controls-', dir=C / 'work') as tmp:
        tmp = Path(tmp)
        fake = tmp / 'fake-hcloud'
        fake.write_text('#!/usr/bin/env python3\n'
                        'import json,pathlib,sys\n'
                        'root=pathlib.Path(__file__).parent\n'
                        'assert sys.argv[2:]==["list","-o","json"]\n'
                        'kind=sys.argv[1]\n'
                        'if (root/"mode").read_text()=="list-failure" and kind=="volume":\n'
                        ' print("synthetic list failure",file=sys.stderr);sys.exit(19)\n'
                        'v=json.loads((root/("before-"+kind+".json")).read_text())\n'
                        'mode=(root/"mode").read_text()\n'
                        'if mode=="extra" and kind=="primary-ip":v.append({"id":999999999999})\n'
                        'if mode=="missing" and kind=="server":v.pop()\n'
                        'print(json.dumps(v))\n')
        fake.chmod(0o700)
        for path in (C / 'inventory').glob('before-*.json'):
            (tmp / path.name).write_bytes(path.read_bytes())
        cases = {}
        for mode in ('same', 'extra', 'missing', 'list-failure'):
            (tmp / 'mode').write_text(mode)
            p = subprocess.run([sys.executable, '-B', str(C / 'verify_inventory.py'),
                                '--inventory', str(C / 'inventory'), '--output-dir', str(tmp / mode),
                                '--hcloud', str(fake)], capture_output=True, text=True)
            want = 0 if mode == 'same' else 1
            assert p.returncode == want, (mode, p.returncode, p.stdout, p.stderr)
            if want:
                assert 'FAIL LOUDLY' in p.stderr
            cases[mode] = {'returncode': p.returncode, 'stdout': p.stdout, 'stderr': p.stderr}
        (C / 'provenance/verify-inventory-controls.json').write_text(json.dumps(cases, indent=2) + '\n')
    print('PASS: verifier accepts identical IDs and rejects an extra primary IP, missing server and failed resource list.')


def main():
    check_commands()
    check_teardown_refusals()
    check_protocol()
    check_inventory_failure_controls()
    return 0


if __name__ == '__main__':
    sys.exit(main())
