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
import json
import os
from pathlib import Path
import sys
import unittest

# abspath, not resolve(): under Bazel stay inside runfiles.
HERE = Path(os.path.abspath(__file__)).parent
sys.path.insert(0, str(HERE))

import placement_receipt as pr  # noqa: E402

GOLDEN_DIR = HERE / 'placementprobe' / 'testdata'
TOOL = 'placementprobe/v1'
ADDRESS_METHOD = ('LocalityGetAddressesForKey: client location cache of a fresh process, NOT versioned; '
                  'bracketed by identical boundaries at read versions V1 and V2')
FIXTURE_SHA256 = 'a' * 64
ROWS = 20
V1, V2 = 1001, 1002
# Shard index (number of global boundaries <= key) -> storage addresses, as the
# Go fake returns them (deliberately unsorted for shard 3; the probe sorts).
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
    return dict(
        format=pr.PROBE_FORMAT, tool=TOOL, store=store, store_prefix_hex=prefix.hex(),
        fixture_receipt_sha256=FIXTURE_SHA256, read_version=V1, boundaries_read_version=V1,
        boundary_range_hex=[prefix.hex(), end.hex()], boundaries_complete=True,
        boundaries_hex=[b.hex() for b in globals_ if prefix <= b < end],
        scanned_subspaces_hex=[record.hex(), index.hex()], keys_complete=True, keys=keys,
        addresses_basis=dict(method=ADDRESS_METHOD, located_after_read_version=V1,
                             located_before_read_version=V2, boundaries_rechecked_identical=True),
        dd_mode=dict(required_off=True, at_v1=dict(read_version=V1, mode=0, key_present=True),
                     at_v2=dict(read_version=V2, mode=0, key_present=True)))


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
                                       chunks=len(pr.RECORD_SUFFIXES[fmt])))
                cells = {'rows10': (0, 10), 'rows20': (0, 20), 'tail': (10, 20)}
                r = pr.build_receipt(status(), probe, cells, **kw)
                self.assertTrue(r['integrity_ok'], r['errors'])
                self.assertTrue(r['passed'])
                self.assertEqual(r['shared_shards'], 0)
                self.assertEqual((r['index_shards'], r['record_shards']), (1, 2))
                self.assertEqual(r['cells']['rows20']['label'], 'cross-server')
                report = pr.run_controls(status(), probe, cells, **kw)
                self.assertTrue(report['ok'], report)

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
