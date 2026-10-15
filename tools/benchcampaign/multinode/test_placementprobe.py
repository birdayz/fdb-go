#!/usr/bin/env python3
"""Compatibility of the Go placement probe (placementprobe/, H5) with placement_receipt.py.

placementprobe/testdata/probe-*.golden.json are the documents the Go probe must
emit for a synthetic cluster; placementprobe/placement_test.go builds that same
cluster as a fake and requires Probe() to reproduce each golden structurally.
This file (a) regenerates each golden from the same synthetic specification in
Python and requires equality, so neither side can drift alone, and (b) feeds
each golden to placement_receipt.build_receipt and run_controls: integrity and
full disjointness must hold, and every injected defect must fail integrity.
Standard library only; temporary fixtures only. Regenerate the goldens after an
intentional format change with: python3 -B test_placementprobe.py --write-goldens
"""
import hashlib
import json
import os
from pathlib import Path
import struct
import sys
import unittest

# abspath, not resolve(): under Bazel stay inside runfiles.
HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import placement_receipt as pr  # noqa: E402

GOLDEN_DIR = HERE / 'placementprobe' / 'testdata'
TOOL = 'placementprobe/v2'
ADDRESS_METHOD = 'LocalityGetAddressesForKey in the V1 snapshot transaction (transactional)'
KEY_SERVERS_DESCRIPTION = ('raw entries from the last keyServers key <= begin up to end; sha256 over '
                           '(uint32 BE length, bytes) of each key and value')
FIXTURE_SHA256 = 'a' * 64
CLUSTER = dict(connection_string_id='fdbbenchvpc:mn20261014', cluster_file_sha256='c' * 64)
WRITE_TOKEN_HEX = bytes(range(16)).hex()
ROWS = 20
V1, V2 = 1001, 1002
# Shard index (number of global boundaries <= key) -> storage addresses; the Go
# fake stores each as a \xff/keyServers value, comma-joined in this order
# (deliberately unsorted for shard 3; the probe sorts).
SHARDS = [
    ['10.88.0.11:4500', '10.88.0.12:4500'],
    ['10.88.0.11:4504', '10.88.0.12:4502'],
    ['10.88.0.12:4503', '10.88.0.13:4502'],
    ['10.88.0.13:4503', '10.88.0.11:4505'],
]
MACHINE = {'10.88.0.11': 'node1', '10.88.0.12': 'node2', '10.88.0.13': 'node3'}
STORES = {
    'benchfetch/small': (('benchfetch', 'small'), 'unsplit'),
    'benchfetch/split': (('benchfetch', 'split'), 'split'),
}


def golden_path(store):
    return GOLDEN_DIR / f'probe-{store.replace("/", "-")}.golden.json'


def synthetic(store):
    """The probe document for the synthetic cluster of placement_test.go."""
    prefix_items, fmt = STORES[store]
    prefix = pr.pack(list(prefix_items))
    record, index = prefix + pr.pack([1]), prefix + pr.pack([2])
    globals_ = sorted([b'\x01', prefix + pr.pack([1, 10]), prefix + pr.pack([1, 1000000])])

    def addresses(key):
        shard = sum(b <= key for b in globals_)
        return sorted(SHARDS[shard])

    keys = []
    for i in range(ROWS):
        for s in pr.RECORD_SUFFIXES[fmt]:
            k = prefix + pr.pack([1, i, s])
            keys.append(dict(kind='record', id=i, key_hex=k.hex(), addresses=addresses(k)))
    for i in range(ROWS):
        k = prefix + pr.pack([2, 'price_idx', i, i])
        keys.append(dict(kind='index', id=i, key_hex=k.hex(), addresses=addresses(k)))
    end = pr.strinc(prefix)
    # keyServers entries the probe reads: the last one <= begin, then those < end.
    entries = [(pr.KEY_SERVERS_PREFIX + b, ','.join(SHARDS[i + 1]).encode()) for i, b in enumerate(globals_)]
    digest = hashlib.sha256()
    for k, v in entries:
        for part in (k, v):
            digest.update(struct.pack('>I', len(part)) + part)
    return dict(
        format=pr.PROBE_FORMAT, tool=TOOL, store=store, store_prefix_hex=prefix.hex(),
        fixture_receipt_sha256=FIXTURE_SHA256, cluster=CLUSTER, read_version=V1, boundaries_read_version=V1,
        boundary_range_hex=[prefix.hex(), end.hex()], boundaries_complete=True,
        boundaries_hex=[b.hex() for b in globals_ if prefix <= b < end],
        scanned_subspaces_hex=[record.hex(), index.hex()], keys_complete=True, keys=keys,
        addresses_basis=dict(kind='transactional', read_version=V1, method=ADDRESS_METHOD),
        recheck=dict(read_version=V2, boundaries_identical=True),
        dd_mode=dict(required_off=True, at_v1=dict(read_version=V1, mode=0, key_present=True),
                     at_v2=dict(read_version=V2, mode=0, key_present=True)),
        move_keys_lock=dict(owner_hex_at_v1=pr.DD_MODE_LOCK_HEX, owner_hex_at_v2=pr.DD_MODE_LOCK_HEX,
                            owner_unchanged=True, write_token_hex_at_v1=WRITE_TOKEN_HEX,
                            write_token_hex_at_v2=WRITE_TOKEN_HEX, write_token_unchanged=True),
        key_servers=dict(begin_hex=(pr.KEY_SERVERS_PREFIX + prefix).hex(), end_hex=(pr.KEY_SERVERS_PREFIX + end).hex(),
                         entries=len(entries), sha256_at_v1=digest.hexdigest(), sha256_at_v2=digest.hexdigest(),
                         unchanged=True, description=KEY_SERVERS_DESCRIPTION))


def status():
    procs = {}
    for i, a in enumerate(sorted({a for s in SHARDS[1:] for a in s})):
        node = MACHINE[a.split(':')[0]]
        procs[f'p{i}'] = dict(address=a, class_type='storage', locality=dict(machineid=node, zoneid=node),
                              roles=[dict(role='storage', id=f's{i}')])
    return dict(cluster=dict(processes=procs))


class GoldenTest(unittest.TestCase):
    def test_goldens_match_the_python_specification(self):
        for store in STORES:
            with self.subTest(store=store):
                self.assertEqual(json.loads(golden_path(store).read_text()), synthetic(store))

    def test_goldens_pass_the_receipt_and_controls(self):
        for store, (prefix_items, fmt) in STORES.items():
            with self.subTest(store=store):
                probe = json.loads(golden_path(store).read_text())
                prefix = pr.pack(list(prefix_items))
                kw = dict(layout='recordlayer', rows=ROWS, record_format=fmt, expect_prefix=prefix,
                          fixture=dict(phase='fixture', prefix_bytes=len(prefix), rows=ROWS,
                                       chunks=len(pr.RECORD_SUFFIXES[fmt])),
                          fixture_sha256=FIXTURE_SHA256, expect_cluster=CLUSTER)
                cells = {'rows10': (0, 10), 'rows20': (0, 20), 'tail': (10, 20)}
                r = pr.build_receipt(status(), probe, cells, **kw)
                self.assertTrue(r['integrity_ok'], r['errors'])
                self.assertTrue(r['passed'])
                self.assertEqual(r['shared_shards'], 0)
                self.assertEqual((r['index_shards'], r['record_shards']), (1, 2))
                self.assertEqual(r['cells']['rows20']['label'], 'cross-server')
                self.assertEqual(pr.sealed_errors(r, CLUSTER), [])
                report = pr.run_controls(status(), probe, cells, **kw)
                self.assertTrue(report['ok'], report)
                self.assertTrue(report['provenance_mutations'])

    def test_lock_owner_may_be_any_stable_value(self):
        """DataDistributor::init takes the lock (random owner, token untouched) before it sees mode 0."""
        store, (prefix_items, fmt) = 'benchfetch/small', STORES['benchfetch/small']
        prefix = pr.pack(list(prefix_items))
        kw = dict(layout='recordlayer', rows=ROWS, record_format=fmt, expect_prefix=prefix,
                  fixture=dict(phase='fixture', prefix_bytes=len(prefix), rows=ROWS, chunks=1),
                  fixture_sha256=FIXTURE_SHA256, expect_cluster=CLUSTER)
        cells = {'rows20': (0, 20)}

        def receipt(**lock):
            probe = synthetic(store)
            probe['move_keys_lock'].update(lock)
            return pr.build_receipt(status(), probe, cells, **kw)
        stable = receipt(owner_hex_at_v1='9e' * 16, owner_hex_at_v2='9e' * 16)
        self.assertTrue(stable['integrity_ok'], stable['errors'])
        self.assertEqual(pr.sealed_errors(stable, CLUSTER), [])
        self.assertEqual(stable['move_keys_owner_hex'], '9e' * 16)
        for name, lock in [('owner changed', dict(owner_hex_at_v1='9e' * 16, owner_hex_at_v2='07' * 16)),
                           ('owner missing', dict(owner_hex_at_v1='', owner_hex_at_v2='')),
                           ('token changed', dict(write_token_hex_at_v2='ff' * 16))]:
            with self.subTest(name):
                r = receipt(**lock)
                self.assertFalse(r['integrity_ok'])
                self.assertTrue(pr.sealed_errors(r, CLUSTER))
        report = pr.run_controls(status(), synthetic(store), cells, **kw)
        self.assertTrue(report['ok'], report)
        self.assertTrue(report['positive_controls']['stable-random-owner']['accepted'])
        self.assertTrue(report['provenance_mutations']['lock-owner-taken']['rejected'])  # owner-only change
        self.assertTrue(report['provenance_mutations']['lock-token-changed']['rejected'])
        sealed, fresh = stable, receipt(owner_hex_at_v1='07' * 16, owner_hex_at_v2='07' * 16)
        self.assertTrue(fresh['integrity_ok'])
        self.assertIn('move_keys_owner_hex differs', pr.compare(sealed, fresh)['differences'])

    def test_probe_ids_and_scope(self):
        probe = synthetic('benchfetch/split')
        record, index = probe['scanned_subspaces_hex']
        for k in probe['keys']:
            self.assertTrue(k['key_hex'].startswith(record if k['kind'] == 'record' else index))
        self.assertEqual(len(probe['keys']), ROWS * 3)


if __name__ == '__main__':
    if sys.argv[1:] == ['--write-goldens']:
        GOLDEN_DIR.mkdir(parents=True, exist_ok=True)
        for s in STORES:
            golden_path(s).write_text(json.dumps(synthetic(s), indent=2) + '\n')
            print(golden_path(s))
        sys.exit(0)
    unittest.main(verbosity=1)
